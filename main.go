//go:build windows

// she - Efficient Shell: run commands on a Linux server from Windows over one
// kept SSH connection.
//
// Commands come only on stdin; the argument is a session name or an option.
//
//	she <<'E'        run the script with a fresh bash on the server
//	cd ~/project     (independent calls, may run in parallel, stdout and
//	git status        stderr apart, the script's exit code)
//	E
//	she NAME <<'E'   run in the bash kept for NAME (an interactive bash on a
//	                 terminal): cd, variables and functions stay; returns 254
//	                 when the command waits for input
//	she NAME <<< 'a' send a to NAME: input to the command running there, or
//	                 a script if none runs
//	she NAME         (typed alone at a console) read a line without showing
//	                 it and send it to the command running in NAME
//	she -start       open the connection now
//	she -stop        close the connection (the next she opens it again)
//	she -setup HOST  the host to use, as ssh knows it (~/.ssh/config); needed once
//
// The first call starts a hidden helper (she -daemon) which keeps one SSH
// connection to the host and one dispatcher on the server (modes.go). The
// helper does not watch the connection; a call that finds it dead opens it
// again, once, before the script starts. A connection lost while a script
// runs is reported (exit 255), not retried. Keys for people typing she (Tab
// in Git Bash and PowerShell) are in completion.bash and completion.ps1.
//
// Host, user, port, key and known_hosts come from `ssh -G HOST` (the user's
// OpenSSH config), and the host key is checked against those known_hosts.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

const (
	frameStdout = 1
	frameStderr = 2
	frameExit   = 3
	frameError  = 4
)

// marker the server's ~/.bashrc uses to tell she from a plain ssh command
const marker = "SHE=1"

type state struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	Pid   int    `json:"pid"`
}

type request struct {
	Token   string `json:"token"`
	Op      string `json:"op"`      // run, stop
	Mode    string `json:"mode"`    // channel, bash, agent (SHE_MODE)
	Session string `json:"session"` // she NAME: a kept bash (agent only), or a host (remote.go)
	Sub     string `json:"sub"`     // she HOST NAME: the NAME
	Script  string `json:"script"`  // the script, or the path for put and get
	Data    string `json:"data"`    // put: the file, base64
}

// %USERPROFILE%\.she, not under AppData: a program started from an MSIX app
// (the Claude desktop app) writes to AppData\Local in the app's own copy
// (Packages\<app>\LocalCache), so a she started there and one started from a
// terminal would read two state files and start two helpers (2026-10-11).
func dir() string {
	base, err := os.UserHomeDir()
	if err != nil {
		base = os.TempDir()
	}
	d := filepath.Join(base, ".she")
	_ = os.MkdirAll(d, 0700)
	return d
}

func hostName() string {
	b, err := os.ReadFile(filepath.Join(dir(), "host"))
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help"):
		fmt.Print(usage)
		os.Exit(0)
	case len(args) == 0 && term.IsTerminal(int(os.Stdin.Fd())):
		// she typed alone at a console: nothing to run
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	case len(args) == 1 && args[0] == "-daemon":
		os.Exit(daemon())
	case len(args) == 1 && args[0] == "-l":
		// the kept bashes: NAME, a tab, running or idle
		os.Exit(run("\x04", ""))
	case len(args) == 1 && args[0] == "-start":
		os.Exit(start())
	case len(args) == 1 && args[0] == "-stop":
		os.Exit(stop())
	case len(args) == 2 && args[0] == "-setup":
		_ = stop()
		if err := os.WriteFile(filepath.Join(dir(), "host"), []byte(args[1]+"\n"), 0600); err != nil {
			fmt.Fprintln(os.Stderr, "she:", err)
			os.Exit(2)
		}
		fmt.Println("she: host", args[1])
		os.Exit(0)
	case len(args) == 3 && args[0] == "--complete":
		// the completions of the text args[2] in the bash kept for args[1]
		// (or, for "", one kept only for completing), one a line
		os.Exit(run("\x01"+args[1], args[2]))
	case (len(args) == 1 || len(args) == 2) && args[0] == "--complete":
		// the same with the text on stdin (PowerShell 5.1 drops an empty
		// argument and breaks one with "): one line, its line end dropped
		text, _ := io.ReadAll(os.Stdin)
		text = bytes.TrimPrefix(text, []byte("\xef\xbb\xbf"))
		text = bytes.TrimSuffix(bytes.TrimSuffix(text, []byte("\n")), []byte("\r"))
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		os.Exit(run("\x01"+name, string(text)))
	case len(args) == 1 && args[0] == "--bash-completion":
		fmt.Print(bashCompletion)
		os.Exit(0)
	case len(args) == 1 && args[0] == "--powershell-completion":
		fmt.Print(powershellCompletion)
		os.Exit(0)
	case names(args) && term.IsTerminal(int(os.Stdin.Fd())):
		// she NAME typed at a console with nothing piped in: read one line
		// without showing it (a password asked for by the running command)
		// and send it as input, only while a command runs in NAME
		fmt.Fprintf(os.Stderr, "she: input for %s (not shown): ", strings.Join(args, " "))
		line, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "she: reading the console:", err)
			os.Exit(2)
		}
		os.Exit(runSub("\x03"+args[0], sub(args), string(line)+"\n"))
	case len(args) == 3 && args[0] == "-put":
		// she -put LOCAL [HOST:]PATH (transfer.go)
		data, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "she:", err)
			os.Exit(2)
		}
		host, path := splitHost(args[2])
		os.Exit(call(request{Op: "put", Session: host, Script: path, Data: base64.StdEncoding.EncodeToString(data)}))
	case len(args) == 3 && args[0] == "-get":
		// she -get [HOST:]PATH LOCAL: written only when it came whole
		host, path := splitHost(args[1])
		var buf bytes.Buffer
		callOut = &buf
		rc := call(request{Op: "get", Session: host, Script: path})
		if rc == 0 {
			if err := os.WriteFile(args[2], buf.Bytes(), 0644); err != nil {
				fmt.Fprintln(os.Stderr, "she:", err)
				rc = 2
			}
		}
		os.Exit(rc)
	case len(args) == 2 && args[0] == "--input":
		// input only, read from stdin (the Git Bash function for mintty)
		text, _ := io.ReadAll(os.Stdin)
		os.Exit(run("\x03"+args[1], string(text)))
	case len(args) == 0 || names(args):
		script, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "she: reading stdin:", err)
			os.Exit(2)
		}
		// PowerShell pipes text with a UTF-8 BOM and CRLF line ends
		script = bytes.TrimPrefix(script, []byte("\xef\xbb\xbf"))
		script = bytes.ReplaceAll(script, []byte("\r\n"), []byte("\n"))
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		os.Exit(runSub(name, sub(args), string(script)))
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// names: she NAME or she HOST NAME
func names(args []string) bool {
	if len(args) < 1 || len(args) > 2 {
		return false
	}
	for _, a := range args {
		if a == "" || a[0] == '-' {
			return false
		}
	}
	return true
}

func sub(args []string) string {
	if len(args) == 2 {
		return args[1]
	}
	return ""
}

const usage = `she: run commands on an ssh host (commands come on stdin)
  Git Bash                      PowerShell
  she <<'E'                     @'
  commands                      commands
  E                             '@ | she
  she NAME <<'E' ... E          @' ... '@ | she NAME     in the bash kept for NAME
  she HOST [NAME] <<'E' ... E   @' ... '@ | she HOST     in a shell kept on HOST (an ssh host:
                                                         PowerShell on Windows, else bash)
  she NAME <<< 'text'           'text' | she NAME        input to what runs in NAME
  she NAME                                               (alone) a password, not shown
  Tab after she <  (or she NAME <) writes the form above; Tab again switches it
  she -put LOCAL [HOST:]PATH    she -get [HOST:]PATH LOCAL    a file, checked by sha256
  she -l   the kept shells (NAME, running or idle)
  she -start | she -stop | she -setup HOST
`

// ---------- client ----------

// Keys for people typing she (Tab makes she <<<” / she <<'E' in Git Bash and
// ” | she / @' '@ | she in PowerShell, completion from the server inside the
// quotes, she NAME reads a password): she --bash-completion and
// she --powershell-completion print them, to be read from ~/.bashrc and the
// PowerShell profile.
//
//go:embed completion.bash
var bashCompletion string

//go:embed completion.ps1
var powershellCompletion string

func readState() (*state, error) {
	b, err := os.ReadFile(filepath.Join(dir(), "state.json"))
	if err != nil {
		return nil, err
	}
	var s state
	return &s, json.Unmarshal(b, &s)
}

func dial() (net.Conn, *state, error) {
	s, err := readState()
	if err != nil {
		return nil, nil, err
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port), time.Second)
	return c, s, err
}

// startDaemon starts the helper, one at a time: calls that find no helper at
// once (parallel first calls) take start.lock in turn, and the later ones find
// the helper the first started, so there is never a second helper with kept
// bashes no other call can see. A lock left by a call that died is taken over
// after 10 s.
func startDaemon() error {
	if hostName() == "" {
		return errors.New("no host set: run she -setup HOST (a host ssh knows, from ~/.ssh/config)")
	}
	lock := filepath.Join(dir(), "start.lock")
	for i := 0; ; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			f.Close()
			break
		}
		if st, e := os.Stat(lock); e == nil && time.Since(st.ModTime()) > 10*time.Second {
			_ = os.Remove(lock)
			continue
		}
		if i >= 300 {
			return fmt.Errorf("another she is starting the helper (%s)", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lock)
	if c, _, err := dial(); err == nil {
		// started by the call that held the lock before
		c.Close()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir(), "state.json"))
	cmd := exec.Command(exe, "-daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000 | 0x00000200} // CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 100; i++ {
		if c, _, err := dial(); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(dir(), "daemon.log"))
	return fmt.Errorf("helper did not start: %s", strings.TrimSpace(string(b)))
}

func send(c net.Conn, req request) error {
	b, _ := json.Marshal(req)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := c.Write(n[:]); err != nil {
		return err
	}
	_, err := c.Write(b)
	return err
}

// callOut is where call writes the stdout frames (a buffer for she -get)
var callOut io.Writer = os.Stdout

func run(name, script string) int {
	return runSub(name, "", script)
}

func runSub(name, sub, script string) int {
	return call(request{Op: "run", Mode: os.Getenv("SHE_MODE"), Session: name, Sub: sub, Script: script})
}

// call sends req to the helper (starting it if none answers) and copies the
// frames back: stdout, stderr, and the exit code it returns
func call(req request) int {
	c, s, err := dial()
	if err != nil {
		if err = startDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "she:", err)
			return 255
		}
		c, s, err = dial()
		if err != nil {
			fmt.Fprintln(os.Stderr, "she:", err)
			return 255
		}
	}
	defer c.Close()
	req.Token = s.Token
	if err := send(c, req); err != nil {
		fmt.Fprintln(os.Stderr, "she:", err)
		return 255
	}
	r := bufio.NewReader(c)
	for {
		var head [5]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			fmt.Fprintln(os.Stderr, "she: helper closed the call:", err)
			return 255
		}
		data := make([]byte, binary.BigEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(r, data); err != nil {
			fmt.Fprintln(os.Stderr, "she: helper closed the call:", err)
			return 255
		}
		switch head[0] {
		case frameStdout:
			callOut.Write(data)
		case frameStderr:
			os.Stderr.Write(data)
		case frameError:
			fmt.Fprintln(os.Stderr, "she:", string(data))
		case frameExit:
			return int(binary.BigEndian.Uint32(data))
		}
	}
}

// she -start: open the connection and start the server's dispatcher now, so
// that the first call does not wait for them
func start() int {
	return call(request{Op: "start"})
}

func stop() int {
	c, s, err := dial()
	if err != nil {
		return 0
	}
	defer c.Close()
	_ = send(c, request{Token: s.Token, Op: "stop"})
	_, _ = io.Copy(io.Discard, c)
	return 0
}

// ---------- daemon ----------

type conn struct {
	mu     sync.Mutex
	client *ssh.Client
	host   string

	amu   sync.Mutex // the dispatcher (mode agent)
	agent *agent
	bmu   sync.Mutex // idle bash (mode bash)
	idle  []*bashWorker

	hmu         sync.Mutex // the hosts the default host knows (remote.go)
	serverHosts map[string]bool
	rmu         sync.Mutex // kept shells on other hosts, by HOST/NAME
	remotes     map[string]*remote
}

var logf = func(format string, a ...any) {}

func daemon() int {
	lf, err := os.OpenFile(filepath.Join(dir(), "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err == nil {
		logf = func(format string, a ...any) {
			fmt.Fprintf(lf, time.Now().Format("15:04:05.000 ")+format+"\n", a...)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logf("listen: %v", err)
		return 1
	}
	tok := make([]byte, 32)
	_, _ = rand.Read(tok)
	st := state{Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok), Pid: os.Getpid()}
	b, _ := json.Marshal(st)
	tmp := filepath.Join(dir(), "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		logf("state: %v", err)
		return 1
	}
	if err := os.Rename(tmp, filepath.Join(dir(), "state.json")); err != nil {
		logf("state: %v", err)
		return 1
	}
	cn := &conn{host: hostName()}
	logf("listening on %d for %s", st.Port, cn.host)
	for {
		c, err := ln.Accept()
		if err != nil {
			logf("accept: %v", err)
			return 1
		}
		go cn.serve(c, st.Token, ln)
	}
}

func frame(w io.Writer, mu *sync.Mutex, kind byte, data []byte) error {
	mu.Lock()
	defer mu.Unlock()
	var head [5]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(data)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

type frameWriter struct {
	w    io.Writer
	mu   *sync.Mutex
	kind byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	if err := frame(f.w, f.mu, f.kind, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func exitCode(w io.Writer, mu *sync.Mutex, code int) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code))
	_ = frame(w, mu, frameExit, b[:])
}

func (cn *conn) serve(c net.Conn, token string, ln net.Listener) {
	defer c.Close()
	var n [4]byte
	if _, err := io.ReadFull(c, n[:]); err != nil {
		return
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > 64<<20 {
		return
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(c, b); err != nil {
		return
	}
	var req request
	if json.Unmarshal(b, &req) != nil || req.Token != token {
		return
	}
	var mu sync.Mutex
	if req.Op == "stop" {
		cn.mu.Lock()
		if cn.client != nil {
			cn.client.Close()
		}
		cn.mu.Unlock()
		logf("stop")
		ln.Close()
		os.Exit(0)
	}
	if req.Op == "put" || req.Op == "get" {
		cn.transfer(c, &mu, req)
		return
	}
	if req.Op == "start" {
		if _, err := cn.getAgent(); err != nil {
			_ = frame(c, &mu, frameError, []byte(err.Error()))
			exitCode(c, &mu, 255)
			return
		}
		exitCode(c, &mu, 0)
		return
	}
	switch req.Mode {
	case "bash":
		cn.runBash(c, &mu, req.Script)
		return
	case "", "agent":
		cn.runNamed(c, &mu, req)
		return
	}
	sess, err := cn.session()
	if err != nil {
		_ = frame(c, &mu, frameError, []byte(err.Error()))
		exitCode(c, &mu, 255)
		return
	}
	defer sess.Close()
	sess.Stdout = frameWriter{c, &mu, frameStdout}
	sess.Stderr = frameWriter{c, &mu, frameStderr}
	err = sess.Run(marker + " exec bash -c " + quote(req.Script))
	var ee *ssh.ExitError
	switch {
	case err == nil:
		exitCode(c, &mu, 0)
	case errors.As(err, &ee):
		exitCode(c, &mu, ee.ExitStatus())
	default:
		_ = frame(c, &mu, frameError, []byte("connection lost while the script ran: "+err.Error()))
		exitCode(c, &mu, 255)
		cn.drop()
	}
}

// runNamed sends a call to where its name points (remote.go route): a kept
// shell on another host, or the dispatcher on the default host (fresh bash,
// kept bash, completion, input, the list). The name may start with the
// operation: \x01 complete, \x03 input only, \x04 list.
func (cn *conn) runNamed(c io.Writer, mu *sync.Mutex, req request) {
	op, name := "", req.Session
	if name != "" && name[0] < 32 {
		op, name = name[:1], name[1:]
	}
	if op == "\x04" {
		if l := cn.remoteList(); l != "" {
			_ = frame(c, mu, frameStdout, []byte(l))
		}
		cn.runAgent(c, mu, req.Session, req.Script)
		return
	}
	if name == "" {
		cn.runAgent(c, mu, req.Session, req.Script)
		return
	}
	bash, host, rname, via, err := cn.route(name, req.Sub)
	if err != nil {
		_ = frame(c, mu, frameError, []byte(err.Error()))
		exitCode(c, mu, 2)
		return
	}
	if host == "" {
		cn.runAgent(c, mu, op+bash, req.Script)
		return
	}
	if op == "\x01" {
		// no completion from other hosts
		exitCode(c, mu, 0)
		return
	}
	r, err := cn.getRemote(host, rname, via)
	if err != nil {
		_ = frame(c, mu, frameError, []byte(err.Error()))
		exitCode(c, mu, 255)
		return
	}
	if op == "\x03" {
		r.smu.Lock()
		busy := r.busy
		r.smu.Unlock()
		if !busy {
			_ = frame(c, mu, frameStderr, []byte("she: "+r.name()+" is not waiting for input; nothing was sent\n"))
			exitCode(c, mu, 1)
			return
		}
	}
	rc := r.call(req.Script, frameWriter{c, mu, frameStdout}, frameWriter{c, mu, frameStderr})
	if rc < 0 {
		cn.rmu.Lock()
		delete(cn.remotes, r.key)
		cn.rmu.Unlock()
		r.close()
		rc = 255
	}
	exitCode(c, mu, rc)
}

// quote makes s one single-quoted shell word
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (cn *conn) drop() {
	cn.mu.Lock()
	if cn.client != nil {
		cn.client.Close()
		cn.client = nil
	}
	cn.mu.Unlock()
	// the dispatcher and kept bash end with the connection: their readers see
	// EOF and mark them dead (not reset here: getAgent holds amu while it
	// opens a session, which may call drop)
}

// session opens a session on the kept connection, opening the connection
// (again) if there is none or the one there does not answer
func (cn *conn) session() (*ssh.Session, error) {
	cn.mu.Lock()
	client := cn.client
	cn.mu.Unlock()
	if client != nil {
		if s, err := newSession(client, 3*time.Second); err == nil {
			return s, nil
		} else {
			logf("kept connection failed (%v), opening again", err)
			cn.drop()
		}
	}
	cn.mu.Lock()
	if cn.client == nil {
		c, err := connect(cn.host)
		if err != nil {
			cn.mu.Unlock()
			return nil, err
		}
		cn.client = c
	}
	client = cn.client
	cn.mu.Unlock()
	return newSession(client, 10*time.Second)
}

func newSession(c *ssh.Client, timeout time.Duration) (*ssh.Session, error) {
	type result struct {
		s   *ssh.Session
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := c.NewSession()
		ch <- result{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(timeout):
		go func() {
			if r := <-ch; r.s != nil {
				r.s.Close()
			}
		}()
		return nil, fmt.Errorf("no answer in %v", timeout)
	}
}

// sshConfig is what `ssh -G host` says
type sshConfig struct {
	host, user, port string
	identities       []string
	knownHosts       []string
	hostKeyAlias     string
}

func readSSHConfig(host string) (*sshConfig, error) {
	out, err := exec.Command("ssh", "-G", host).Output()
	if err != nil {
		return nil, fmt.Errorf("ssh -G %s: %v", host, err)
	}
	cfg := &sshConfig{port: "22"}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "hostname":
			cfg.host = f[1]
		case "user":
			cfg.user = f[1]
		case "port":
			cfg.port = f[1]
		case "identityfile":
			cfg.identities = append(cfg.identities, expand(f[1]))
		case "userknownhostsfile":
			for _, k := range f[1:] {
				cfg.knownHosts = append(cfg.knownHosts, expand(k))
			}
		case "hostkeyalias":
			cfg.hostKeyAlias = f[1]
		}
	}
	return cfg, nil
}

// expand turns ~ and MSYS paths (/c/Users/...) into Windows paths
func expand(p string) string {
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(p, "~") {
		p = filepath.Join(home, p[1:])
	}
	if len(p) > 3 && p[0] == '/' && p[2] == '/' {
		p = strings.ToUpper(p[1:2]) + ":" + p[2:]
	}
	return filepath.FromSlash(p)
}

func connect(host string) (*ssh.Client, error) {
	cfg, err := readSSHConfig(host)
	if err != nil {
		return nil, err
	}
	var signers []ssh.Signer
	for _, id := range cfg.identities {
		b, err := os.ReadFile(id)
		if err != nil {
			continue
		}
		if s, err := ssh.ParsePrivateKey(b); err == nil {
			signers = append(signers, s)
		}
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no usable key among %v (keys with a passphrase are not used)", cfg.identities)
	}
	var files []string
	for _, k := range cfg.knownHosts {
		if _, err := os.Stat(k); err == nil {
			files = append(files, k)
		}
	}
	check, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %v", err)
	}
	addr := net.JoinHostPort(cfg.host, cfg.port)
	algos := hostKeyAlgorithms(files, cfg.host, cfg.port)
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:              cfg.user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback:   check,
		HostKeyAlgorithms: algos,
		Timeout:           10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %v", addr, err)
	}
	logf("connected to %s as %s", addr, cfg.user)
	return c, nil
}

// hostKeyAlgorithms lists the key types known_hosts holds for the host, so
// the server is asked for a key that can be checked (the Go client otherwise
// may take one known_hosts does not have and report a mismatch)
func hostKeyAlgorithms(files []string, host, port string) []string {
	names := []string{host}
	if port != "22" {
		names = []string{"[" + host + "]:" + port}
	}
	seen := map[string]bool{}
	var algos []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			fields := strings.Fields(string(line))
			if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			for _, h := range strings.Split(fields[0], ",") {
				for _, n := range names {
					if h == n && !seen[fields[1]] {
						seen[fields[1]] = true
						switch fields[1] {
						case "ssh-ed25519":
							algos = append(algos, ssh.KeyAlgoED25519)
						case "ssh-rsa":
							algos = append(algos, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
						default:
							algos = append(algos, fields[1])
						}
					}
				}
			}
		}
	}
	return algos
}
