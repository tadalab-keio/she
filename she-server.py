#!/usr/bin/python3
# she on the server: the same use as she on a PC, from an ssh login of the
# same user (in place of tmux), for the bashes that she on a PC keeps
# (2026-10-12, ~/lab-tools/she/).
#
#   she <<'E' ... E      run with a fresh bash here (no rc files, stdin empty)
#   she NAME <<'E' ...   run in the bash kept for NAME, or, while a command
#   she NAME <<< 'text'  there waits for input (it returned 254), send it as
#                        input (a lone control character such as $'\x03',
#                        Ctrl-C, goes without the newline <<< adds)
#   she NAME             (typed alone) read a line without showing it (a
#                        password) and send it to the command running in NAME
#   she -l               the kept bashes (NAME, running or idle)
#   she --complete NAME TEXT   completions of TEXT (for Tab; NAME may be "")
#
# A bash is kept only by she on a PC (she NAME there makes it); this one
# reaches them through the socket each dispatcher has in RUNDIR, which only
# this user can open. Tab: server-completion.bash.
import getpass, glob, json, os, socket, struct, sys, tempfile

uid = os.getuid()
RUNDIR = "/run/user/%d/she" % uid if os.path.isdir("/run/user/%d" % uid) else os.path.join(tempfile.gettempdir(), "she-%d" % uid)

USAGE = """she: run commands on this server, or in the bashes she on a PC keeps
  she <<'E'                 run with a fresh bash here
  commands
  E
  she NAME <<'E' ... E      run in the bash kept for NAME
  she NAME <<< 'text'       the same, or input to what waits in NAME
  she NAME                  (alone) a password, not shown
  she -l                    the kept bashes (NAME, running or idle)
  Tab after she <  (or she NAME <) writes she <<<''; Tab again switches to <<'E'
"""

def ask(path, req):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(None)
    s.connect(path)
    s.sendall((json.dumps(req) + "\n").encode())
    return s

def socks():
    return sorted(glob.glob(os.path.join(RUNDIR, "*.sock")))

def kept():
    # {name: (state, socket path)} over every dispatcher that answers
    found = {}
    for path in socks():
        try:
            s = ask(path, {"op": "list"})
            s.settimeout(5)
            data = b""
            while True:
                d = s.recv(65536)
                if not d:
                    break
                data += d
            s.close()
        except OSError:
            continue
        for row in data.decode().splitlines():
            name, state = row.split("\t")
            found[name] = (state, path)
    return found

def frames(s):
    # copy the answer to stdout / stderr; the exit code
    rc, buf = 255, b""
    while True:
        d = s.recv(65536)
        if not d:
            return rc
        buf += d
        while len(buf) >= 5:
            kind, n = struct.unpack(">BI", buf[:5])
            if len(buf) < 5 + n:
                break
            body, buf = buf[5:5 + n], buf[5 + n:]
            if kind == 1:
                sys.stdout.buffer.write(body)
                sys.stdout.flush()
            elif kind == 2:
                sys.stderr.buffer.write(body)
                sys.stderr.flush()
            elif kind == 3:
                rc = struct.unpack(">i", body)[0]

def keys(text):
    # <<< adds a newline; a lone control character goes without it
    if len(text) == 2 and ord(text[0]) < 32 and text[1] == "\n":
        return text[0]
    return text

def to(name, op, text):
    k = kept()
    if name not in k:
        print("she: no bash is kept for %s (she -l lists them; she NAME on a PC makes one)" % name, file=sys.stderr)
        return 1
    return frames(ask(k[name][1], {"op": op, "name": name, "text": text}))

def main():
    a = sys.argv[1:]
    if a in (["-h"], ["--help"]):
        print(USAGE, end="")
        return 0
    if a == ["-l"]:
        for name, (state, _) in sorted(kept().items()):
            print("%s\t%s" % (name, state))
        return 0
    if len(a) == 3 and a[0] == "--complete":
        if a[1]:
            return to(a[1], "complete", a[2])
        for path in socks():
            try:
                return frames(ask(path, {"op": "complete", "name": "", "text": a[2]}))
            except OSError:
                continue
        return 1
    if len(a) > 1 or (a and a[0].startswith("-")):
        print(USAGE, end="", file=sys.stderr)
        return 2
    if not a:
        if sys.stdin.isatty():
            print(USAGE, end="", file=sys.stderr)
            return 2
        script = sys.stdin.read()
        sys.stdout.flush()
        null = os.open(os.devnull, os.O_RDONLY)
        os.dup2(null, 0)
        os.execvp("bash", ["bash", "--norc", "--noprofile", "-c", script])
    name = a[0]
    if sys.stdin.isatty():
        return to(name, "input", getpass.getpass("she: input for %s (not shown): " % name) + "\n")
    return to(name, "run", keys(sys.stdin.read()))

sys.exit(main())
