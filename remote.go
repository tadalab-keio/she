//go:build windows

package main

// Kept shells on other ssh hosts: she HOST [NAME].
//
// HOST is a host the default host (she -setup) or this PC knows from its
// ~/.ssh/config. The shell runs on a terminal: through the default host's ssh
// (ssh -tt HOST on it, over the kept connection) when the default host knows
// HOST, else straight from this PC (connect, as for the default host). Both
// take the same path over the network when the PC reaches the lab by its VPN,
// whose end is the default host, and the default host keeps the lab's keys
// and ssh config in one place (2026-10-15: a new connection to the B1500A
// took 0.67-1.24 s from the PC's ssh and 0.55-0.89 s through the server).
//
// On Windows the shell is PowerShell whatever the host's ssh starts (cmd on
// the B1500A); elsewhere bash. A script goes as one line that unpacks it
// (base64), so it may span lines and hold any quotes. Before the script the
// line prints a begin marker and after it the prompt prints an end marker
// with the exit status, both with a random token, so what the terminal shows
// before (the echoed line) is dropped and the call returns as the command
// ends. Output that stops without a line end for 1 s is taken as a command
// waiting for input (exit 254), as for the kept bashes on the default host.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const remoteQuiet = time.Second

type remote struct {
	key, host string
	via       bool
	tok       string
	win       bool
	sess      *ssh.Session
	client    *ssh.Client // a connection of its own (straight from this PC)
	in        io.WriteCloser

	mu   sync.Mutex // one call at a time
	bmu  sync.Mutex // buf, dead, last
	cv   *sync.Cond
	buf  []byte
	dead bool
	last time.Time

	smu  sync.Mutex
	busy bool // a command runs (a call returned 254 and it waits for input)
	n    int
	held []byte
}

type sink struct{ r *remote }

func (s sink) Write(p []byte) (int, error) {
	s.r.bmu.Lock()
	s.r.buf = append(s.r.buf, p...)
	s.r.last = time.Now()
	s.r.bmu.Unlock()
	s.r.cv.Broadcast()
	return len(p), nil
}

// control sequences (colours, cursor moves, erase, window title, modes)
var ctrlSeq = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[()][0-9A-Za-z]|[=>78DEHMNOc])")

func (r *remote) plain(b []byte) []byte {
	b = append(r.held, b...)
	r.held = nil
	if k := bytes.LastIndexByte(b, 0x1b); k >= 0 && len(b)-k < 64 {
		if loc := ctrlSeq.FindIndex(b[k:]); loc == nil || loc[0] != 0 {
			r.held = append([]byte(nil), b[k:]...)
			b = b[:k]
		}
	}
	b = ctrlSeq.ReplaceAll(b, nil)
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// take waits up to d for output and takes what came; quiet tells how long
// nothing came, dead whether the shell ended
func (r *remote) take(d time.Duration) (b []byte, quiet time.Duration, dead bool) {
	r.bmu.Lock()
	defer r.bmu.Unlock()
	if len(r.buf) == 0 && !r.dead {
		t := time.AfterFunc(d, r.cv.Broadcast)
		r.cv.Wait()
		t.Stop()
	}
	b, r.buf = r.buf, nil
	return b, time.Since(r.last), r.dead
}

func (r *remote) write(b []byte) error {
	_, err := r.in.Write(b)
	return err
}

// until reads output (without control sequences) until re matches, the
// shell ends, or max passes
func (r *remote) until(re *regexp.Regexp, max time.Duration) error {
	var acc []byte
	t0 := time.Now()
	for {
		b, _, dead := r.take(50 * time.Millisecond)
		acc = append(acc, r.plain(b)...)
		if re.Match(acc) {
			r.held = nil
			return nil
		}
		if dead {
			return fmt.Errorf("the shell on %s ended: %s", r.host, strings.TrimSpace(string(acc)))
		}
		if time.Since(t0) > max {
			return fmt.Errorf("no answer from %s in %v: %s", r.host, max, strings.TrimSpace(string(acc)))
		}
	}
}

// quiet reads output until nothing comes for d (at most max)
func (r *remote) quiet(d, max time.Duration) []byte {
	var acc []byte
	t0 := time.Now()
	for time.Since(t0) < max {
		b, q, dead := r.take(50 * time.Millisecond)
		acc = append(acc, b...)
		if dead || (len(acc) > 0 && q >= d) {
			break
		}
	}
	return acc
}

// A prompt may follow other text on its line: a Windows console moves the
// cursor instead of sending a line break, and that move is taken out.
var (
	winBanner = regexp.MustCompile(`Microsoft Windows|Windows PowerShell|(?m)^PS [^\n]*>\s*$|(?m)[A-Za-z]:\\[^\n]*>\s*$`)
	psPrompt  = regexp.MustCompile(`(?m)\bPS [^\n]*>\s*$`)
)

func newRemote(cn *conn, host, name string, via bool) (*remote, error) {
	tok := make([]byte, 8)
	_, _ = rand.Read(tok)
	r := &remote{key: host + "/" + name, host: host, via: via, tok: hex.EncodeToString(tok), last: time.Now()}
	r.cv = sync.NewCond(&r.bmu)
	var sess *ssh.Session
	var err error
	if via {
		sess, err = cn.session()
	} else {
		r.client, err = connect(host)
		if err == nil {
			sess, err = newSession(r.client, 10*time.Second)
		}
	}
	if err != nil {
		return nil, err
	}
	r.sess = sess
	// wide, so a long line (the unpacking one) does not wrap on a Windows
	// console, which draws what it shows
	if err = sess.RequestPty("dumb", 50, 4000, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		r.close()
		return nil, err
	}
	sess.Stdout, sess.Stderr = sink{r}, sink{r}
	if r.in, err = sess.StdinPipe(); err != nil {
		r.close()
		return nil, err
	}
	if via {
		err = sess.Start(marker + " exec ssh -tt -o BatchMode=yes -o ConnectTimeout=10 " + quote(host))
	} else {
		err = sess.Shell()
	}
	if err != nil {
		r.close()
		return nil, err
	}
	go func() {
		_ = sess.Wait()
		r.bmu.Lock()
		r.dead = true
		r.bmu.Unlock()
		r.cv.Broadcast()
	}()
	// a Windows console may first send only control sequences
	var first []byte
	for t0 := time.Now(); len(bytes.TrimSpace(first)) == 0; {
		left := 20*time.Second - time.Since(t0)
		if left <= 0 {
			break
		}
		b := r.quiet(remoteQuiet, left)
		if len(b) == 0 {
			break
		}
		first = append(first, r.plain(b)...)
	}
	r.held = nil
	r.win = winBanner.Match(first)
	if r.win {
		// Windows PowerShell may print its prompt a second or more after
		// its banner; another shell (cmd) starts one
		if !psPrompt.Match(first) {
			if !bytes.Contains(first, []byte("Windows PowerShell")) {
				_ = r.write([]byte("powershell -NoLogo -NoProfile\r"))
			}
			if err = r.until(psPrompt, 20*time.Second); err != nil {
				r.close()
				return nil, err
			}
		}
		// no line editing (it redraws the line); UTF-8 out; the prompt
		// prints the end marker, built from parts so the echoed line does
		// not hold it
		init := "Remove-Module PSReadLine -ErrorAction SilentlyContinue; [Console]::OutputEncoding=[Text.Encoding]::UTF8; " +
			"$global:__she_tok='" + r.tok + "'; $global:__she_id='init'; " +
			"$global:__she_ok=$true; function global:prompt { $c = if ($global:__she_ok) { 0 } elseif ($global:LASTEXITCODE) { $global:LASTEXITCODE } else { 1 }; " +
			"('@@she-'+'end-'+$global:__she_tok+' '+$global:__she_id+' '+$c+' '+$PWD.Path+[char]10+'PS> ') }\r"
		_ = r.write([]byte(init))
	} else {
		init := "stty -echo; PS1=; PS2=; __she_tok=" + r.tok + "; __she_id=init; " +
			`PROMPT_COMMAND='__she_c=$?; printf "@@she-end-%s %s %s %s\n" "$__she_tok" "$__she_id" "$__she_c" "$PWD"'` + "\r"
		_ = r.write([]byte(init))
	}
	if err = r.until(r.endRe("init"), 20*time.Second); err != nil {
		r.close()
		return nil, err
	}
	r.held = nil
	return r, nil
}

func (r *remote) endRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`@@she-end-` + r.tok + ` ` + regexp.QuoteMeta(id) + ` (-?\d+) ([^\r\n]*)\r?\n`)
}

func (r *remote) close() {
	if r.sess != nil {
		r.sess.Close()
	}
	if r.client != nil {
		r.client.Close()
	}
}

// chunk is how much base64 one typed line carries: a console line has a
// limit (cmd's 8191 characters) and a Windows console wraps at its width
const chunk = 3000

// lines are the lines typed to run script: the base64 of the script is
// gathered into a variable in pieces of at most chunk, then the last line
// prints the begin marker and runs it. The pieces' prompts print end markers
// of id "c", before the begin marker, which a call does not look at.
func (r *remote) lines(id, script string) string {
	if r.win {
		// the status is the last command's ($? right after it; a native
		// command's exit code from $LASTEXITCODE), false when the script
		// stops on an error: the prompt sees only that the script ran
		script = "$global:__she_ok=$false; $global:LASTEXITCODE=0\n" + script + "\n$global:__she_ok=$?"
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	var b strings.Builder
	for i := 0; i < len(b64); i += chunk {
		j := min(i+chunk, len(b64))
		op := "+="
		if i == 0 {
			op = "="
		}
		if r.win {
			b.WriteString("$global:__she_id='c'; $global:__she_b" + op + "'" + b64[i:j] + "'\r")
		} else {
			b.WriteString("__she_id=c; __she_b" + op + "'" + b64[i:j] + "'\r")
		}
	}
	if r.win {
		b.WriteString("$global:__she_id='" + id + "'; Write-Host ('@@she-'+'beg-'+$global:__she_tok); " +
			". ([ScriptBlock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($global:__she_b))))\r")
	} else {
		b.WriteString("__she_id=" + id + `; printf '%s\n' "@@she-beg-$__she_tok"; eval "$(printf %s "$__she_b" | base64 -d)"` + "\r")
	}
	return b.String()
}

// typed is input for a running command as typed at a keyboard (Enter is CR)
func typed(s string) []byte {
	s = strings.ReplaceAll(s, "\r\n", "\r")
	return []byte(strings.ReplaceAll(s, "\n", "\r"))
}

// call runs script (or, while a command waits for input, sends it as input)
// and writes the output to w: the exit status, 254 when the command waits
// for input, -1 when the shell ended
func (r *remote) call(script string, out, errw io.Writer) int {
	if len(script) == 1 && script[0] < 32 {
		// a lone control character (Ctrl-C) goes at once
		r.smu.Lock()
		busy := r.busy
		r.smu.Unlock()
		if busy {
			_ = r.write([]byte(script))
			return 0
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.smu.Lock()
	busy := r.busy
	var id string
	if !busy {
		if strings.TrimSpace(script) == "" {
			r.smu.Unlock()
			return 0
		}
		id = fmt.Sprintf("%d", r.n)
		r.n++
		r.busy = true
	} else {
		id = fmt.Sprintf("%d", r.n-1)
	}
	r.smu.Unlock()
	if busy {
		if script != "" {
			_ = r.write(typed(script))
		}
	} else {
		r.bmu.Lock()
		r.buf = nil // what the terminal showed since (the prompt)
		r.bmu.Unlock()
		_ = r.write([]byte(r.lines(id, script)))
	}
	return r.collect(id, busy, out, errw)
}

func (r *remote) collect(id string, started bool, out, errw io.Writer) int {
	beg := regexp.MustCompile(`@@she-beg-` + r.tok + `\r?\n`)
	end := r.endRe(id)
	hold := len(r.tok) + 40
	// acc is output without control sequences: a Windows console may put
	// them anywhere, also inside a marker's line
	var acc []byte
	lastNL := true
	emit := func(b []byte) {
		if len(b) > 0 {
			out.Write(b)
			lastNL = b[len(b)-1] == '\n'
		}
	}
	for {
		b, q, dead := r.take(50 * time.Millisecond)
		acc = append(acc, r.plain(b)...)
		if !started {
			if loc := beg.FindIndex(acc); loc != nil {
				acc = acc[loc[1]:]
				started = true
			} else if dead {
				break
			} else {
				continue
			}
		}
		if m := end.FindSubmatchIndex(acc); m != nil {
			emit(acc[:m[0]])
			rc := 0
			fmt.Sscanf(string(acc[m[2]:m[3]]), "%d", &rc)
			where := string(acc[m[4]:m[5]])
			r.held = nil
			r.smu.Lock()
			r.busy = false
			r.smu.Unlock()
			if !lastNL {
				fmt.Fprintln(errw)
			}
			fmt.Fprintf(errw, "[%s] %s\n", r.host, where)
			return rc & 0xff
		}
		if dead {
			break
		}
		if len(acc) > hold {
			// keep a tail that may be the start of the end marker
			k := len(acc) - hold
			if i := bytes.LastIndex(acc[:len(acc)], []byte("@@she-")); i >= 0 && i < k {
				k = i
			}
			emit(acc[:k])
			acc = acc[k:]
		}
		if q >= remoteQuiet {
			ends := lastNL
			if len(acc) > 0 {
				ends = acc[len(acc)-1] == '\n'
			}
			if !ends && bytes.Index(acc, []byte("@@she-")) < 0 {
				emit(acc)
				acc = nil
				fmt.Fprintf(errw, "\nshe: waiting for input (a prompt (no newline, quiet for 1 s)): answer with she %s <<< 'text', wait on with she %s </dev/null, or stop it with she %s <<< $'\\x03'\n", r.name(), r.name(), r.name())
				return 254
			}
		}
	}
	emit(acc)
	fmt.Fprintf(errw, "she: the shell on %s ended; it starts afresh next time\n", r.host)
	return -1
}

func (r *remote) name() string {
	h, n, _ := strings.Cut(r.key, "/")
	if h == n {
		return h
	}
	return h + " " + n
}

// ---------- which host a name is ----------

// hostsIn lists the Host names (no patterns) of an ssh config text
func hostsIn(text string) []string {
	var hs []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 2 || !strings.EqualFold(f[0], "host") {
			continue
		}
		for _, h := range f[1:] {
			if !strings.ContainsAny(h, "*?!") {
				hs = append(hs, h)
			}
		}
	}
	return hs
}

// knownHosts: the hosts the default host knows (its ~/.ssh/config, asked
// once) and the hosts this PC knows
func (cn *conn) knownHosts() (server, pc map[string]bool) {
	cn.hmu.Lock()
	defer cn.hmu.Unlock()
	if cn.serverHosts == nil {
		cn.serverHosts = map[string]bool{}
		if s, err := cn.session(); err == nil {
			b, _ := s.Output(marker + " exec cat ~/.ssh/config")
			s.Close()
			for _, h := range hostsIn(string(b)) {
				cn.serverHosts[h] = true
			}
		}
	}
	pc = map[string]bool{}
	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, ".ssh", "config")); err == nil {
		for _, h := range hostsIn(string(b)) {
			pc[h] = true
		}
	}
	return cn.serverHosts, pc
}

// route tells where she TARGET [SUB] goes: a kept bash on the default host
// (bash != ""), or a kept shell on another host (host != "")
func (cn *conn) route(target, sub string) (bash, host, name string, via bool, err error) {
	server, pc := cn.knownHosts()
	isHost := func(h string) bool { return h != cn.host && (server[h] || pc[h]) }
	switch {
	case target == cn.host:
		if sub != "" && isHost(sub) {
			return "", "", "", false, fmt.Errorf("%s is an ssh host: a kept bash on %s cannot take that name (she %s is the shell on %s)", sub, cn.host, sub, sub)
		}
		if sub == "" {
			sub = target
		}
		return sub, "", "", false, nil
	case isHost(target):
		if sub == "" {
			sub = target
		}
		return "", target, sub, server[target], nil
	case sub != "":
		return "", "", "", false, fmt.Errorf("%s is not an ssh host (in the ~/.ssh/config of %s or of this PC)", target, cn.host)
	}
	return target, "", "", false, nil
}

func (cn *conn) getRemote(host, name string, via bool) (*remote, error) {
	key := host + "/" + name
	cn.rmu.Lock()
	r := cn.remotes[key]
	cn.rmu.Unlock()
	if r != nil {
		r.bmu.Lock()
		dead := r.dead
		r.bmu.Unlock()
		if !dead {
			return r, nil
		}
		r.close()
	}
	r, err := newRemote(cn, host, name, via)
	if err != nil {
		return nil, err
	}
	cn.rmu.Lock()
	if cn.remotes == nil {
		cn.remotes = map[string]*remote{}
	}
	cn.remotes[key] = r
	cn.rmu.Unlock()
	return r, nil
}

// remoteList: HOST NAME (or HOST), a tab, running or idle, a line each
func (cn *conn) remoteList() string {
	cn.rmu.Lock()
	defer cn.rmu.Unlock()
	var lines []string
	for _, r := range cn.remotes {
		r.bmu.Lock()
		dead := r.dead
		r.bmu.Unlock()
		if dead {
			continue
		}
		r.smu.Lock()
		st := "idle"
		if r.busy {
			st = "running"
		}
		r.smu.Unlock()
		lines = append(lines, r.name()+"\t"+st)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
