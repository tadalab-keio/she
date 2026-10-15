//go:build windows

package main

// she -put LOCAL [HOST:]PATH and she -get [HOST:]PATH LOCAL: a file over the
// kept connection, checked by sha256 on both ends (as rsh put/get did, which
// she had left out). On the default host through a session of its own
// (cat, sha256sum); on another host through the shell kept for it (she
// HOST), as a script that holds the file in base64.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
)

// splitHost: "b1500a:C:\x" is host b1500a, path C:\x; "C:\x" and "/x" have
// no host (a drive letter is one character)
func splitHost(s string) (host, path string) {
	if i := strings.Index(s, ":"); i > 1 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// shPath is a shell word for p on the default host (~/ is the home)
func shPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		return `"$HOME"/` + quote(p[2:])
	}
	return quote(p)
}

// psQuote is a single-quoted PowerShell string
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (cn *conn) transfer(c io.Writer, mu *sync.Mutex, req request) {
	fail := func(format string, a ...any) {
		_ = frame(c, mu, frameError, []byte(fmt.Sprintf(format, a...)))
		exitCode(c, mu, 1)
	}
	var data []byte
	if req.Op == "put" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(req.Data); err != nil {
			fail("put: %v", err)
			return
		}
	}
	path := req.Script
	host := req.Session
	if host == "" || host == cn.host {
		cn.transferDefault(c, mu, req.Op, path, data, fail)
		return
	}
	_, h, _, via, err := cn.route(host, "")
	if err != nil || h == "" {
		fail("%s is not an ssh host (in the ~/.ssh/config of %s or of this PC)", host, cn.host)
		return
	}
	r, err := cn.getRemote(h, h, via)
	if err != nil {
		fail("%v", err)
		return
	}
	r.smu.Lock()
	busy := r.busy
	r.smu.Unlock()
	if busy {
		fail("the shell kept for %s runs a command (it waits for input): answer or stop it first", h)
		return
	}
	if via {
		cn.transferVia(c, mu, r, req.Op, path, data, fail)
		return
	}
	var script string
	if r.win {
		p := "$ErrorActionPreference = 'Stop'\n$p = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath(" + psQuote(path) + ")\n"
		if req.Op == "put" {
			script = p + "$d = [IO.Path]::GetDirectoryName($p); if ($d) { [void][IO.Directory]::CreateDirectory($d) }\n" +
				"[IO.File]::WriteAllBytes($p, [Convert]::FromBase64String('" + base64.StdEncoding.EncodeToString(data) + "'))\n" +
				"(Get-FileHash -Algorithm SHA256 -LiteralPath $p).Hash.ToLower()"
		} else {
			// short lines: a Windows console garbles a long one
			script = p + "[Convert]::ToBase64String([IO.File]::ReadAllBytes($p), 'InsertLineBreaks')\n" +
				"(Get-FileHash -Algorithm SHA256 -LiteralPath $p).Hash.ToLower()"
		}
	} else {
		p := "p=" + shPath(path) + "\n"
		if req.Op == "put" {
			script = p + `mkdir -p -- "$(dirname -- "$p")" && printf %s '` + base64.StdEncoding.EncodeToString(data) + `' | base64 -d > "$p" && sha256sum -- "$p" | cut -c1-64`
		} else {
			script = p + `base64 -w0 -- "$p" && echo && sha256sum -- "$p" | cut -c1-64`
		}
	}
	var out, errb bytes.Buffer
	rc := r.call(script, &out, &errb)
	if rc != 0 {
		if rc == 254 {
			_ = r.write([]byte{3})
		}
		fail("%s on %s failed (%d): %s%s", req.Op, h, rc, strings.TrimSpace(out.String()), strings.TrimSpace(errb.String()))
		return
	}
	lines := strings.Fields(out.String())
	if len(lines) == 0 {
		fail("%s on %s: no answer", req.Op, h)
		return
	}
	sum := lines[len(lines)-1]
	if req.Op == "put" {
		if sum != shaHex(data) {
			fail("put to %s: sha256 differs (sent %s, there %s)", h, shaHex(data), sum)
			return
		}
		_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: put %d bytes to %s:%s (sha256 %s)\n", len(data), h, path, sum[:16])))
		exitCode(c, mu, 0)
		return
	}
	got, err := base64.StdEncoding.DecodeString(strings.Join(lines[:len(lines)-1], ""))
	if err != nil || shaHex(got) != sum {
		fail("get from %s: the data did not come whole (sha256 there %s)", h, sum)
		return
	}
	_ = frame(c, mu, frameStdout, got)
	_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: got %d bytes from %s:%s (sha256 %s)\n", len(got), h, path, sum[:16])))
	exitCode(c, mu, 0)
}

// onDefault runs cmd with bash on the default host, in its own session,
// with in on its stdin; stdout, or an error with its stderr
func (cn *conn) onDefault(cmd string, in []byte) ([]byte, error) {
	s, err := cn.session()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if in != nil {
		s.Stdin = bytes.NewReader(in)
	}
	var e bytes.Buffer
	s.Stderr = &e
	out, err := s.Output(marker + " exec bash -c " + quote(cmd))
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(e.String()))
	}
	return out, nil
}

// transferVia moves the file with the default host's scp (SFTP) to or from
// a host it knows: a Windows console takes typed text slowly (200 KB took
// 18 s) and garbles long output. The shell kept for the host makes the
// folder and tells the sha256 there.
func (cn *conn) transferVia(c io.Writer, mu *sync.Mutex, r *remote, op, path string, data []byte, fail func(string, ...any)) {
	tok := make([]byte, 6)
	_, _ = rand.Read(tok)
	tmp := `"$HOME/.cache/she-transfer/` + hex.EncodeToString(tok) + `"`
	far := path
	if r.win {
		far = strings.ReplaceAll(path, `\`, "/")
	}
	far = quote(r.host + ":" + far)
	ask := func(script string) (string, error) {
		var out, errb bytes.Buffer
		rc := r.call(script, &out, &errb)
		if rc == 254 {
			_ = r.write([]byte{3})
		}
		if rc != 0 {
			return "", fmt.Errorf("on %s (%d): %s", r.host, rc, strings.TrimSpace(out.String()))
		}
		f := strings.Fields(out.String())
		if len(f) == 0 {
			return "", fmt.Errorf("on %s: no answer", r.host)
		}
		return f[len(f)-1], nil
	}
	var p, mkdir, sum string
	if r.win {
		p = "$ErrorActionPreference = 'Stop'\n$p = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath(" + psQuote(path) + ")\n"
		mkdir = p + "$d = [IO.Path]::GetDirectoryName($p); if ($d) { [void][IO.Directory]::CreateDirectory($d) }\n'ok'"
		sum = p + "(Get-FileHash -Algorithm SHA256 -LiteralPath $p).Hash.ToLower()"
	} else {
		p = "p=" + shPath(path) + "\n"
		mkdir = p + `mkdir -p -- "$(dirname -- "$p")" && echo ok`
		sum = p + `sha256sum -- "$p" | cut -c1-64`
	}
	scp := "mkdir -p ~/.cache/she-transfer && "
	if op == "put" {
		if _, err := ask(mkdir); err != nil {
			fail("put: %v", err)
			return
		}
		if _, err := cn.onDefault(scp+"cat > "+tmp+" && scp -s -q -o BatchMode=yes "+tmp+" "+far+"; rc=$?; rm -f "+tmp+"; exit $rc", data); err != nil {
			fail("put: %v", err)
			return
		}
		there, err := ask(sum)
		if err != nil || there != shaHex(data) {
			fail("put to %s: sha256 differs (sent %s, there %s %v)", r.host, shaHex(data), there, err)
			return
		}
		_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: put %d bytes to %s:%s (sha256 %s)\n", len(data), r.host, path, there[:16])))
		exitCode(c, mu, 0)
		return
	}
	got, err := cn.onDefault(scp+"scp -s -q -o BatchMode=yes "+far+" "+tmp+" && cat "+tmp+"; rc=$?; rm -f "+tmp+"; exit $rc", nil)
	if err != nil {
		fail("get: %v", err)
		return
	}
	there, err := ask(sum)
	if err != nil || there != shaHex(got) {
		fail("get from %s: sha256 differs (there %s, came %s %v)", r.host, there, shaHex(got), err)
		return
	}
	_ = frame(c, mu, frameStdout, got)
	_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: got %d bytes from %s:%s (sha256 %s)\n", len(got), r.host, path, there[:16])))
	exitCode(c, mu, 0)
}

func (cn *conn) transferDefault(c io.Writer, mu *sync.Mutex, op, path string, data []byte, fail func(string, ...any)) {
	p := "p=" + shPath(path) + "; "
	run := func(cmd string, in []byte) ([]byte, error) { return cn.onDefault(p+cmd, in) }
	if op == "put" {
		out, err := run(`mkdir -p -- "$(dirname -- "$p")" && cat > "$p.she-put" && mv -f -- "$p.she-put" "$p" && sha256sum -- "$p" | cut -c1-64`, data)
		if err != nil {
			fail("put: %v", err)
			return
		}
		if sum := strings.TrimSpace(string(out)); sum != shaHex(data) {
			fail("put: sha256 differs (sent %s, there %s)", shaHex(data), sum)
			return
		}
		_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: put %d bytes to %s:%s (sha256 %s)\n", len(data), cn.host, path, shaHex(data)[:16])))
		exitCode(c, mu, 0)
		return
	}
	got, err := run(`cat -- "$p"`, nil)
	if err != nil {
		fail("get: %v", err)
		return
	}
	out, err := run(`sha256sum -- "$p" | cut -c1-64`, nil)
	if err != nil {
		fail("get: %v", err)
		return
	}
	if sum := strings.TrimSpace(string(out)); sum != shaHex(got) {
		fail("get: sha256 differs (there %s, came %s)", sum, shaHex(got))
		return
	}
	_ = frame(c, mu, frameStdout, got)
	_ = frame(c, mu, frameStderr, []byte(fmt.Sprintf("she: got %d bytes from %s:%s (sha256 %s)\n", len(got), cn.host, path, shaHex(got)[:16])))
	exitCode(c, mu, 0)
}
