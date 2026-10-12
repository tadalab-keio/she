//go:build windows

package main

// The three ways a call runs on the server (SHE_MODE), kept side by side to
// compare them (2026-10-11):
//
//	channel  a new SSH session per call running `bash -c` (sshd starts the
//	         user's login shell each time, which reads ~/.bashrc and
//	         /etc/bashrc again)
//	bash     bash kept running in up to 8 sessions; a call goes to an idle
//	         one and runs in a subshell `( ... )`, so cd and variables do not
//	         stay; calls beyond the idle ones open another
//	agent    one small Python dispatcher kept in one session; each call is a
//	         bare `bash --norc --noprofile -c` it starts, with the
//	         environment the dispatcher got once

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ---------- agent ----------

const agentCode = `import os,sys,struct,subprocess,threading,pty,termios,fcntl,tempfile,time,socket,json
inp=sys.stdin.buffer
out=sys.stdout.buffer
lock=threading.Lock()
# calls from she on the server itself (a socket, see serve) have ids below 0
sinks={}
def send(i,k,d):
    c=sinks.get(i)
    if c is not None:
        try:
            c.sendall(struct.pack(">BI",k,len(d))+d)
        except OSError:
            pass
        return
    with lock:
        out.write(struct.pack(">IBI",i,k,len(d))+d)
        out.flush()
def pump(i,k,f):
    while True:
        d=os.read(f.fileno(),65536)
        if not d:
            break
        send(i,k,d)
def run(i,s):
    try:
        p=subprocess.Popen(["bash","--norc","--noprofile","-c",s],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    except Exception as e:
        send(i,2,str(e).encode())
        send(i,3,struct.pack(">i",127))
        return
    t=[threading.Thread(target=pump,args=(i,1,p.stdout)),threading.Thread(target=pump,args=(i,2,p.stderr))]
    for x in t:
        x.start()
    for x in t:
        x.join()
    rc=p.wait()
    if rc<0:
        rc=128-rc
    send(i,3,struct.pack(">i",rc))
SENT=b"\x00@@she-end-"
KDIR=tempfile.mkdtemp(prefix="she-")
QUIET=0.05
QUIET_PROMPT=1.0
# poll, select, epoll_wait, pselect6, ppoll, epoll_pwait, epoll_pwait2
POLLS={7:1,23:0,232:-1,270:0,271:1,281:-1,441:-1}
# Sourced first in a kept bash. The end marker comes from PROMPT_COMMAND,
# which runs before every prompt, so it comes also when Ctrl-C cuts the line;
# it carries the call's id (a marker of an earlier line is dropped), the exit
# code and the prompt (PS1 as set by ~/.bashrc), and the sourced files are
# removed there.
INIT=r'''__she_ps1=$PS1
set +H; PS1=; PS2=; PS0=; unset HISTFILE
__she_pc() {
  local rc=$? p=${__she_ps1@P}
  rm -f -- %s/*
  printf '\0@@she-end-%s %%s %%d %%s\n' "$__she_id" "$rc" "${p//$'\n'/ }"
}
PROMPT_COMMAND=__she_pc
# a pager (less) only waits for keys here: print instead
export PAGER=cat GIT_PAGER=cat
# she --complete: the completions of the line in file $1, each as the whole
# line, from the completion bash uses at the prompt (bash-completion); a
# directory ends in /, and a lone directory gives its entries instead, so
# readline does not close the quote after it.
__she_complete() {
  local l h= r d x re='^(.*[;&|(][[:space:]]*)(.*)$'
  local -a a
  l=$(<"$1")
  # complete the last command of the line (after ; && || | or ()
  if [[ $l =~ $re ]]; then
    h=${BASH_REMATCH[1]} l=${BASH_REMATCH[2]}
  fi
  r=$( (__she_complete1 "$l") 2>/dev/null )
  if [[ $r == */ && $r != *$'\n'* ]]; then
    d=$( (__she_complete1 "$r") 2>/dev/null )
    [[ $d == *$'\n'* ]] && r=$d
  fi
  [[ -n $r ]] || return 0
  mapfile -t a <<< "$r"
  for x in "${a[@]}"; do printf '%%s%%s\n' "$h" "$x"; done
}
__she_complete1() {
  local line=$1 cmd spec cur prev o c e pre
  local COMP_LINE=$1 COMP_POINT=${#1} COMP_WORDS COMP_CWORD COMPREPLY=()
  read -ra COMP_WORDS <<< "$line"
  [[ -z $line || $line == *[[:space:]] ]] && COMP_WORDS+=("")
  COMP_CWORD=$((${#COMP_WORDS[@]}-1))
  cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]}
  pre=${line%%"$cur"}
  # bash-completion leaves ~ to readline, which is not here: ask with $HOME
  # and give ~ back
  local tl=
  if [[ $cur == "~/"* ]]; then
    tl=1 cur=$HOME/${cur:2}
    COMP_WORDS[COMP_CWORD]=$cur COMP_LINE=$pre$cur
    COMP_POINT=${#COMP_LINE}
  fi
  if ((COMP_CWORD==0)); then
    mapfile -t COMPREPLY < <(compgen -A function -A alias -A builtin -A keyword -c -- "$cur")
  else
    cmd=${COMP_WORDS[0]}
    spec=$(complete -p -- "$cmd" 2>/dev/null)
    if [[ -z $spec ]] && declare -F _completion_loader >/dev/null; then
      _completion_loader "$cmd"
      spec=$(complete -p -- "$cmd" 2>/dev/null)
    fi
    if [[ $spec =~ -F\ ([^ ]+) ]]; then
      "${BASH_REMATCH[1]}" "$cmd" "$cur" "$prev"
    elif [[ -n $spec ]]; then
      o=${spec#complete }
      o=${o%% *}
      eval "mapfile -t COMPREPLY < <(compgen $o -- \"\$cur\")"
    else
      mapfile -t COMPREPLY < <(compgen -f -- "$cur")
    fi
  fi
  for c in "${COMPREPLY[@]}"; do
    e=$c
    [[ $e == "~"* ]] && e=$HOME${e:1}
    ((COMP_CWORD)) && [[ $c != */ && -d $e ]] && c+=/
    [[ $tl && $c == "$HOME/"* ]] && c="~/${c#"$HOME/"}"
    printf '%%s\n' "$pre$c"
  done | sort -u
}
'''
def keys(data):
    # <<< adds a newline; a lone control character (Ctrl-C) goes without it
    if len(data)==2 and data[0]<32 and data[1]==10:
        return data[:1]
    return data
class Kept:
    # A bash kept for a name (she NAME): cd, variables and functions stay. It
    # is an interactive bash (bash -i) on its own terminal (pty), so ~/.bashrc,
    # aliases and the reboot notice work as when logged in, a command can ask
    # for input, also from the terminal as sudo and ssh do, and Ctrl-C stops
    # it (user, 2026-10-11). stdout and stderr come as one stream, as on a
    # terminal, and what is typed in is echoed (a password is not). A script
    # is written to a file and sourced, so its text never passes the terminal.
    # A call returns when the command ends, with its exit code and the prompt
    # on stderr (it shows the user and the directory), or returns 254 when
    # the command waits for input (see waiting); the next call with the name
    # then sends its text to the command as input (she NAME <<< 'text'), or
    # with no text (she NAME </dev/null) waits on. A long command is waited
    # for as long as it runs. Calls with one name run in turn; only a lone
    # control character (Ctrl-C) and input (she NAME alone, or she NAME on
    # the server) reach a running command at once. What the bash prints when
    # it starts (the reboot
    # notice) goes with the first call. History expansion (!) is off and
    # nothing is written to ~/.bash_history.
    def __init__(self,name):
        self.name=name
        self.m,s=pty.openpty()
        self.tty=os.ttyname(s)
        at=termios.tcgetattr(s)
        at[1]&=~termios.ONLCR
        termios.tcsetattr(s,termios.TCSANOW,at)
        def child():
            os.setsid()
            fcntl.ioctl(0,termios.TIOCSCTTY,0)
        env=dict(os.environ,TERM="dumb")
        self.p=subprocess.Popen(["bash","--noediting","-i"],stdin=s,stdout=s,stderr=s,preexec_fn=child,env=env,close_fds=True)
        os.close(s)
        self.lock=threading.Lock()
        self.st=threading.Lock()
        self.attached=False
        self.cv=threading.Condition()
        self.raw=bytearray()
        self.acc=b""
        self.pending=b""
        self.busy=False
        self.tok=os.urandom(8).hex()
        self.cur=None
        self.echo=None
        self.dir=tempfile.mkdtemp(dir=KDIR)
        self.dead=False
        self.n=0
        self.last_nl=True
        threading.Thread(target=self.pump,daemon=True).start()
        init=os.path.join(self.dir,"init")
        with open(init,"w") as f:
            f.write(INIT%(self.dir,self.tok))
        with self.lock:
            self.start("init",". "+init)
            got=[]
            self.collect(got.append,init=True)
            text=b"".join(got)
            # drop the prompt printed before the init line was read
            k=text.rfind(b"\n")
            self.pending=text[:k+1] if k>=0 else b""
    def pump(self):
        while True:
            try:
                d=os.read(self.m,65536)
            except OSError:
                d=b""
            with self.cv:
                if not d:
                    self.dead=True
                    self.cv.notify_all()
                    return
                self.raw+=d
                self.cv.notify_all()
    def start(self,cid,line):
        self.cur=cid
        with self.st:
            self.busy=True
        self.last_nl=True
        self.echo=("__she_id="+cid+"; "+line+"\n").encode()
        os.write(self.m,self.echo)
    def waiting(self,idle,ends_nl):
        # Whether the running command waits for input, from its state, not
        # from how long it is quiet: a program turned echo off (a password
        # prompt), or a process of the foreground job is in read() on the
        # terminal. A process that cannot be looked into (another user's, as
        # sudo ...) or that polls the terminal with others (telnet) counts
        # only when its output stops without a newline for 1 s (a prompt such
        # as "Is this ok [y/N]: "). A command that just runs (sleep, a build)
        # is waited for.
        # Echo off with line input (ICANON) is a password prompt; sudo keeps
        # the terminal raw (echo and ICANON off) for as long as its command
        # runs (use_pty), whatever the command does, so raw tells nothing.
        try:
            lf=termios.tcgetattr(self.m)[3]
            if not lf&termios.ECHO and lf&termios.ICANON:
                return "a password prompt (echo off)"
            pg=os.tcgetpgrp(self.m)
        except OSError:
            return None
        guess=False
        for pid in os.listdir("/proc"):
            if not pid.isdigit():
                continue
            try:
                with open("/proc/%s/stat"%pid) as f:
                    if int(f.read().rsplit(")",1)[1].split()[2])!=pg:
                        continue
            except (OSError,ValueError,IndexError):
                continue
            try:
                with open("/proc/%s/syscall"%pid) as f:
                    sc=f.read().split()
            except OSError:
                guess=True
                continue
            if not sc or not sc[0].isdigit():
                continue
            n=int(sc[0])
            try:
                if n==0:
                    if os.readlink("/proc/%s/fd/%d"%(pid,int(sc[1],16))) in (self.tty,"/dev/tty"):
                        return "reading the terminal"
                elif n in POLLS and (POLLS[n]<0 or int(sc[1+POLLS[n]],16)>0):
                    for x in os.listdir("/proc/%s/fd"%pid):
                        if os.readlink("/proc/%s/fd/%s"%(pid,x)) in (self.tty,"/dev/tty"):
                            guess=True
                            break
            except (OSError,ValueError,IndexError):
                pass
        if guess and not ends_nl and idle>=QUIET_PROMPT:
            return "a prompt (no newline, quiet for 1 s)"
        return None
    def collect(self,emit,init=False):
        # forward output to emit until the end marker: (exit code, prompt),
        # (-2, why) when the command waits for input, None when bash ended
        t0=last=time.time()
        head=SENT+(self.tok+" ").encode()
        hold=len(head)
        check=0
        def out(b):
            if b:
                emit(b)
                self.last_nl=b.endswith(b"\n")
        while True:
            with self.cv:
                if not self.raw and not self.dead:
                    self.cv.wait(0.02)
                d=bytes(self.raw)
                self.raw.clear()
                dead=self.dead
            now=time.time()
            if d:
                last=now
                self.acc+=d
            if self.echo:
                k=self.acc.find(self.echo)
                if k>=0:
                    self.acc=self.acc[:k]+self.acc[k+len(self.echo):]
                    self.echo=None
                elif now-t0>1 or dead:
                    self.echo=None
            while True:
                j=self.acc.find(head)
                nl=self.acc.find(b"\n",j) if j>=0 else -1
                if nl<0:
                    break
                f=self.acc[j+len(head):nl].decode("utf-8","replace").split(" ",2)
                if f[0]==self.cur:
                    out(self.acc[:j])
                    self.acc=self.acc[nl+1:]
                    self.echo=None
                    with self.st:
                        self.busy=False
                    return (int(f[1]),f[2].rstrip() if len(f)>2 else "")
                self.acc=self.acc[:j]+self.acc[nl+1:]
            if dead:
                out(self.acc)
                self.acc=b""
                return None
            if self.echo:
                continue
            j=self.acc.find(head)
            cut=j if j>=0 else len(self.acc)-hold
            if cut>0:
                out(self.acc[:cut])
                self.acc=self.acc[cut:]
            if init or now-last<QUIET or now<check:
                continue
            # look at once, then less often the longer it is quiet
            check=now+max(QUIET,(now-last)/2)
            ends_nl=self.acc.endswith(b"\n") if self.acc else self.last_nl
            why=self.waiting(now-last,ends_nl)
            if why:
                out(self.acc)
                self.acc=b""
                return (-2,why)
    def call(self,i,s):
        data=keys(s.encode("utf-8","surrogateescape"))
        # While another call is attached to a running command, a lone control
        # character (Ctrl-C) goes to it at once; anything else waits its turn
        # (calls with one name run in turn), and goes as input only if the
        # command then waits for input (the attached call returned 254).
        with self.st:
            inject=self.attached and self.busy and not self.dead and len(data)==1 and data[0]<32
            if inject:
                os.write(self.m,data)
                self.last_nl=True
        if inject:
            send(i,3,struct.pack(">i",0))
            return True
        with self.lock:
            with self.st:
                self.attached=True
            try:
                return self.attach(i,data)
            finally:
                with self.st:
                    self.attached=False
    def attach(self,i,data):
        if self.dead:
            return False
        if self.pending:
            send(i,1,self.pending)
            self.pending=b""
        if self.busy:
            if data:
                os.write(self.m,data)
                self.last_nl=True
        elif not data:
            send(i,3,struct.pack(">i",0))
            return True
        else:
            cid="%d"%self.n
            self.n+=1
            path=os.path.join(self.dir,cid)
            with open(path,"wb") as f:
                f.write(data)
            self.start(cid,". "+path)
        r=self.collect(lambda d:send(i,1,d))
        if r is None:
            rc=self.p.wait()
            if rc<0:
                rc=128-rc
            send(i,2,b"she: the kept bash ended (exit or a signal); it starts afresh next time\n")
        elif r[0]==-2:
            n=self.name
            send(i,2,("she: waiting for input (%s): answer with she %s <<< 'text', wait on with she %s </dev/null, or stop it with she %s <<< $'\\x03'\n"%(r[1],n,n,n)).encode())
            rc=254
        else:
            rc,prompt=r
            if prompt:
                send(i,2,(b"" if self.last_nl else b"\n")+prompt.encode()+b"\n")
        send(i,3,struct.pack(">i",rc))
        return True
    def input(self,i,s):
        # she NAME typed alone at a console, or she NAME on the server: the
        # text goes to the running command as input, only while one runs (in
        # an idle bash it would run as a command); the output stays for the
        # call that waits on it (she NAME </dev/null)
        data=keys(s.encode("utf-8","surrogateescape"))
        with self.st:
            ok=self.busy and not self.dead
            if ok:
                os.write(self.m,data)
                self.last_nl=True
        if ok:
            send(i,2,("she: sent to %s\n"%self.name).encode())
        else:
            send(i,2,("she: %s is not waiting for input; nothing was sent\n"%self.name).encode())
        send(i,3,struct.pack(">i",0 if ok else 1))
    def complete(self,i,text):
        # she --complete: only while the bash is idle (never as input to a
        # running command); prints the completions of text, one a line
        rc=1
        if self.lock.acquire(blocking=False):
            try:
                if not self.busy and not self.dead:
                    cid="%d"%self.n
                    self.n+=1
                    path=os.path.join(self.dir,cid)
                    with open(path,"wb") as f:
                        f.write(text.encode("utf-8","surrogateescape"))
                    self.start(cid,"__she_complete "+path)
                    got=[]
                    r=self.collect(got.append,init=True)
                    if r:
                        send(i,1,b"".join(got))
                        rc=r[0]
            finally:
                self.lock.release()
        send(i,3,struct.pack(">i",rc))
kept={}
klock=threading.Lock()
def listing():
    # the kept bashes: NAME, a tab, running (a command runs in it) or idle
    with klock:
        rows=["%s\t%s"%(n,"running" if k.busy else "idle") for n,k in sorted(kept.items()) if n!="\x02" and not k.dead]
    return "".join(r+"\n" for r in rows).encode()
def run_named(i,name,s):
    if name=="\x04":
        # she -l
        send(i,1,listing())
        send(i,3,struct.pack(">i",0))
        return
    if name.startswith("\x03"):
        # input only (she NAME alone at a console)
        with klock:
            k=kept.get(name[1:])
        if k is None or k.dead:
            send(i,2,("she: no bash is kept for %s\n"%name[1:]).encode())
            send(i,3,struct.pack(">i",1))
        else:
            k.input(i,s)
        return
    if name.startswith("\x01"):
        # she --complete [NAME]: in that name's bash, or for no name in a
        # bash kept only for this (it never runs a script, so stays at ~)
        name=name[1:] or "\x02"
        with klock:
            k=kept.get(name)
            if k is None or k.dead:
                k=kept[name]=Kept(name)
        k.complete(i,s)
        return
    for _ in range(2):
        with klock:
            k=kept.get(name)
            if k is None or k.dead:
                k=kept[name]=Kept(name)
        if k.call(i,s):
            with klock:
                if k.dead and kept.get(name) is k:
                    del kept[name]
            return
    send(i,2,b"she: could not start the kept bash\n")
    send(i,3,struct.pack(">i",255))
# A socket only this user can open, so that she on the server (an ssh login
# of the same user) can use the kept bashes as she on the PC does: list them,
# run in one, send input, complete; one per dispatcher (per PC helper).
# A request is one JSON line (op, name, text); the answer comes as frames of
# kind (1 stdout, 2 stderr, 3 exit code) and length, or for list as text.
uid=os.getuid()
RUNDIR="/run/user/%d/she"%uid if os.path.isdir("/run/user/%d"%uid) else os.path.join(tempfile.gettempdir(),"she-%d"%uid)
SOCK=os.path.join(RUNDIR,"%d.sock"%os.getpid())
nid=[0]
def local(c):
    try:
        req=json.loads(c.makefile("rb").readline())
        op=req.get("op")
        if op=="list":
            c.sendall(listing())
            return
        name=req.get("name") or ""
        with klock:
            if op=="complete" and not name:
                # as she --complete on the PC: a bash kept only for this
                name="\x02"
                if name not in kept or kept[name].dead:
                    kept[name]=Kept(name)
            k=kept.get(name)
            nid[0]-=1
            i=nid[0]
        sinks[i]=c
        try:
            if k is None or k.dead:
                send(i,2,("she: no bash is kept for %s\n"%name).encode())
                send(i,3,struct.pack(">i",1))
            elif op=="input":
                k.input(i,req["text"])
            elif op=="run":
                # a script, or input to a command that waits for it (in turn)
                if not k.call(i,req["text"]):
                    send(i,2,b"she: the kept bash ended\n")
                    send(i,3,struct.pack(">i",255))
            elif op=="complete":
                k.complete(i,req["text"])
        finally:
            sinks.pop(i,None)
    except Exception:
        pass
    finally:
        c.close()
def serve():
    try:
        os.makedirs(RUNDIR,0o700,exist_ok=True)
        s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
        s.bind(SOCK)
        os.chmod(SOCK,0o600)
        s.listen(8)
    except OSError:
        return
    while True:
        c,_=s.accept()
        threading.Thread(target=local,args=(c,),daemon=True).start()
threading.Thread(target=serve,daemon=True).start()
def rd(n):
    b=b""
    while len(b)<n:
        c=inp.read(n-len(b))
        if not c:
            try:
                os.unlink(SOCK)
            except OSError:
                pass
            os._exit(0)
        b+=c
    return b
while True:
    i,nl=struct.unpack(">II",rd(8))
    name=rd(nl).decode()
    n,=struct.unpack(">I",rd(4))
    s=rd(n).decode("utf-8","surrogateescape")
    if name:
        threading.Thread(target=run_named,args=(i,name,s),daemon=True).start()
    else:
        threading.Thread(target=run,args=(i,s),daemon=True).start()
`

type agentCall struct {
	w    io.Writer
	mu   *sync.Mutex
	done chan int
}

type agent struct {
	wmu   sync.Mutex
	stdin io.WriteCloser
	mu    sync.Mutex
	calls map[uint32]*agentCall
	next  uint32
	dead  bool
}

func (cn *conn) getAgent() (*agent, error) {
	cn.amu.Lock()
	defer cn.amu.Unlock()
	if cn.agent != nil {
		cn.agent.mu.Lock()
		dead := cn.agent.dead
		cn.agent.mu.Unlock()
		if !dead {
			return cn.agent, nil
		}
		cn.agent = nil
	}
	sess, err := cn.session()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	if err := sess.Start(marker + " exec python3 -u -c " + quote(agentCode)); err != nil {
		return nil, err
	}
	a := &agent{stdin: stdin, calls: map[uint32]*agentCall{}}
	go func() {
		var head [9]byte
		for {
			if _, err := io.ReadFull(stdout, head[:]); err != nil {
				break
			}
			id := binary.BigEndian.Uint32(head[0:4])
			data := make([]byte, binary.BigEndian.Uint32(head[5:9]))
			if _, err := io.ReadFull(stdout, data); err != nil {
				break
			}
			a.mu.Lock()
			c := a.calls[id]
			if head[4] == 3 {
				delete(a.calls, id)
			}
			a.mu.Unlock()
			if c == nil {
				continue
			}
			switch head[4] {
			case 1:
				_ = frame(c.w, c.mu, frameStdout, data)
			case 2:
				_ = frame(c.w, c.mu, frameStderr, data)
			case 3:
				c.done <- int(int32(binary.BigEndian.Uint32(data)))
			}
		}
		a.mu.Lock()
		a.dead = true
		for id, c := range a.calls {
			delete(a.calls, id)
			c.done <- -1
		}
		a.mu.Unlock()
		logf("agent ended: %s", bytes.TrimSpace(errBuf.Bytes()))
		sess.Close()
	}()
	cn.agent = a
	return a, nil
}

func (cn *conn) runAgent(w io.Writer, mu *sync.Mutex, name, script string) {
	for try := 0; try < 2; try++ {
		a, err := cn.getAgent()
		if err != nil {
			_ = frame(w, mu, frameError, []byte(err.Error()))
			exitCode(w, mu, 255)
			return
		}
		c := &agentCall{w: w, mu: mu, done: make(chan int, 1)}
		a.mu.Lock()
		if a.dead {
			a.mu.Unlock()
			continue
		}
		a.next++
		id := a.next
		a.calls[id] = c
		a.mu.Unlock()
		// id, name (she -s NAME, or empty), script
		msg := make([]byte, 0, 12+len(name)+len(script))
		msg = binary.BigEndian.AppendUint32(msg, id)
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(name)))
		msg = append(msg, name...)
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(script)))
		msg = append(msg, script...)
		a.wmu.Lock()
		_, err = a.stdin.Write(msg)
		a.wmu.Unlock()
		if err != nil {
			// not sent: open again and send once more
			a.mu.Lock()
			delete(a.calls, id)
			a.dead = true
			a.mu.Unlock()
			cn.drop()
			continue
		}
		code := <-c.done
		if code == -1 {
			_ = frame(w, mu, frameError, []byte("connection lost while the script ran"))
			code = 255
			cn.drop()
		}
		exitCode(w, mu, code)
		return
	}
	_ = frame(w, mu, frameError, []byte("could not start the dispatcher"))
	exitCode(w, mu, 255)
}

// ---------- bash pool ----------

const maxBash = 8

var sentinel = []byte("\x00@@she-end-")

type bashWorker struct {
	stdin  io.WriteCloser
	mu     sync.Mutex
	w      io.Writer
	wmu    *sync.Mutex
	id     string
	code   int
	outEnd chan struct{}
	errEnd chan struct{}
	dead   bool
}

func (cn *conn) getBash() (*bashWorker, error) {
	cn.bmu.Lock()
	for i, b := range cn.idle {
		cn.idle = append(cn.idle[:i], cn.idle[i+1:]...)
		b.mu.Lock()
		dead := b.dead
		b.mu.Unlock()
		if !dead {
			cn.bmu.Unlock()
			return b, nil
		}
		break
	}
	cn.bmu.Unlock()
	sess, err := cn.session()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := sess.Start(marker + " exec bash"); err != nil {
		return nil, err
	}
	b := &bashWorker{stdin: stdin}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.pump(stdout, frameStdout) }()
	go func() { defer wg.Done(); b.pump(stderr, frameStderr) }()
	go func() {
		wg.Wait()
		b.mu.Lock()
		b.dead = true
		b.mu.Unlock()
		sess.Close()
	}()
	return b, nil
}

func (cn *conn) putBash(b *bashWorker) {
	cn.bmu.Lock()
	defer cn.bmu.Unlock()
	if len(cn.idle) < maxBash {
		cn.idle = append(cn.idle, b)
	} else {
		b.stdin.Close()
	}
}

// pump forwards what bash writes to the current call, up to the end marker
func (b *bashWorker) pump(r io.Reader, kind byte) {
	buf := make([]byte, 65536)
	var pend []byte
	for {
		n, err := r.Read(buf)
		pend = append(pend, buf[:n]...)
		for {
			b.mu.Lock()
			w, wmu, id := b.w, b.wmu, b.id
			b.mu.Unlock()
			i := bytes.Index(pend, sentinel)
			if i < 0 {
				keep := len(sentinel) - 1
				if len(pend) > keep {
					if w != nil {
						_ = frame(w, wmu, kind, pend[:len(pend)-keep])
					}
					pend = append([]byte(nil), pend[len(pend)-keep:]...)
				}
				break
			}
			if i > 0 && w != nil {
				_ = frame(w, wmu, kind, pend[:i])
			}
			pend = pend[i:]
			nl := bytes.IndexByte(pend, '\n')
			if nl < 0 {
				break
			}
			line := string(pend[len(sentinel):nl])
			pend = pend[nl+1:]
			var gotID string
			code := 0
			if kind == frameStdout {
				if _, err := fmt.Sscanf(line, "%s %d", &gotID, &code); err != nil {
					continue
				}
			} else {
				gotID = line
			}
			if gotID != id {
				continue
			}
			b.mu.Lock()
			if kind == frameStdout {
				b.code = code
				close(b.outEnd)
			} else {
				close(b.errEnd)
			}
			b.mu.Unlock()
		}
		if err != nil {
			b.mu.Lock()
			b.dead = true
			if b.outEnd != nil {
				select {
				case <-b.outEnd:
				default:
					b.code = -1
					close(b.outEnd)
				}
			}
			if b.errEnd != nil {
				select {
				case <-b.errEnd:
				default:
					close(b.errEnd)
				}
			}
			b.mu.Unlock()
			return
		}
	}
}

func (cn *conn) runBash(w io.Writer, mu *sync.Mutex, script string) {
	var b *bashWorker
	var err error
	for try := 0; try < 2; try++ {
		if b, err = cn.getBash(); err != nil {
			break
		}
		rb := make([]byte, 8)
		_, _ = rand.Read(rb)
		id := hex.EncodeToString(rb)
		b.mu.Lock()
		b.w, b.wmu, b.id, b.code = w, mu, id, 0
		b.outEnd, b.errEnd = make(chan struct{}), make(chan struct{})
		b.mu.Unlock()
		cmd := "( eval \"$(printf %s '" + base64.StdEncoding.EncodeToString([]byte(script)) + "' | base64 -d)\" ) </dev/null; " +
			"printf '\\0@@she-end-%s %d\\n' " + id + " $?; printf '\\0@@she-end-%s\\n' " + id + " >&2\n"
		if _, err = io.WriteString(b.stdin, cmd); err != nil {
			b.mu.Lock()
			b.dead = true
			b.mu.Unlock()
			continue
		}
		<-b.outEnd
		<-b.errEnd
		b.mu.Lock()
		code := b.code
		b.w, b.id = nil, ""
		dead := b.dead
		b.mu.Unlock()
		if code == -1 {
			_ = frame(w, mu, frameError, []byte("connection lost while the script ran"))
			exitCode(w, mu, 255)
			cn.drop()
			return
		}
		if !dead {
			cn.putBash(b)
		}
		exitCode(w, mu, code)
		return
	}
	if err == nil {
		err = errors.New("could not start bash")
	}
	_ = frame(w, mu, frameError, []byte(err.Error()))
	exitCode(w, mu, 255)
}
