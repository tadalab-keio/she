# she

Run commands on an SSH host from Windows, with the commands given on stdin.

`she` keeps one SSH connection to the host and runs each script you pipe to it there. It was written for coding agents (Claude Code, Codex) driving a Linux server from a Windows PC: no quoting through two shells, no new SSH handshake per command, scripts that can run side by side, kept shells whose `cd` and variables carry over, and a clear signal when a command waits for input such as a password. Tab completion makes it usable by hand too.

## Usage

The commands always come on stdin. The arguments are only a session name or an option.

| Git Bash | PowerShell | What it does |
|---|---|---|
| `she <<'E'`<br>`commands`<br>`E` | `@'`<br>`commands`<br>`'@ \| she` | **Unnamed**: a fresh `bash` each time (no rc files, empty stdin). Calls can run side by side. stdout and stderr stay separate; the exit code is the script's |
| `she NAME <<'E' … E` | `@' … '@ \| she NAME` | **Named**: runs in the bash kept for NAME, so `cd`, variables and functions carry over to the next call |
| `she NAME <<< 'y'` | `'y' \| she NAME` | If the command running in NAME waits for input, sends this as its input; otherwise runs it as a script |
| `she NAME` (alone) | same | Reads one line without showing it (a password) and sends it to the command running in NAME |
| `she -l` | same | The kept sessions (NAME, `running` or `idle`) |
| `she -start` / `she -stop` | same | Open the connection now / close it (the next call opens it again) |
| `she -setup HOST` | same | The host to use, as `ssh` knows it (`~/.ssh/config`). Needed once |

- The connection is opened by the first call and reused. Nothing watches it; a call that finds it dead opens it again before the script starts. **A connection lost while a script runs is not retried** (exit code 255).
- Host name, user, port, key and known hosts come from `ssh -G HOST`, and the host key is checked against those known hosts. The key must work without a password.

### Named sessions

Each name keeps one interactive `bash` with a terminal on the host: `~/.bashrc`, aliases and the password prompts of `sudo`, `su` and `ssh` work.

| When | she |
|---|---|
| the command ends | returns its exit code and prints the prompt on **stderr** (so `$(...)` gets clean output) |
| the command waits for input | returns **exit code 254** and says what it waits for. Then `she NAME <<< 'answer'` sends input, `she NAME </dev/null` waits for the rest without sending any, `she NAME <<< $'\x03'` sends Ctrl+C |
| a long command with no output | waits until it ends (no timeout) |
| `exit` | that bash ends; the next call makes a new one |

How waiting for input is told (from the state of the terminal, not from time):

| State | Taken as |
|---|---|
| echo off, line input | a password prompt (`su`, `getpass`, `sudo`'s own prompt) |
| the foreground process reads from the terminal | waiting for input (`read`, `cat`, an interactive `python3`) |
| a process it can't see into (run by `sudo`, another user's) or one waiting on the terminal and something else (`telnet`) | output that stops for 1 s without a line end (`Is this ok [y/N]: `) |

- Calls to the same name run **in order**. Only a single control character (Ctrl+C) and the hidden input of `she NAME` get through at once.
- Left out of the terminal: line editing (readline), history (`!` expansion, `~/.bash_history`), colours (`TERM=dumb`) and pagers (`PAGER=cat`, `GIT_PAGER=cat`). stdout and stderr come as one stream. Full-screen programs (`vim`, `top`) wait for keys and return 254.

## For agents

- Send commands with `she <<'E'` … `E`. The quoted heredoc keeps the local shell from expanding anything.
- Use a named session (`she w <<'E'`) only when `cd` or variables must carry over, and don't call the same name side by side.
- **On exit code 254**, read the last line (the prompt) and answer. **Never type a password**: ask the user to run `she NAME` and type it, then get the rest with `she NAME </dev/null`.

## Typing by hand (Tab)

| Type | Git Bash (and the host's interactive shell) | PowerShell |
|---|---|---|
| `she <` then Tab | `she <<<'│'` (`│` is the cursor) | `'│' \| she` |
| Tab / Shift+Tab again | switches to `she <<'E'` / `│` / `E` and back | switches to `@'` / `│` / `'@ \| she` and back |
| `she NAME <` then Tab | the same, keeping NAME | same |
| Tab inside the quotes or the body (`cd ./l`) | completes from the host's bash (from NAME's bash and directory for a named session) | same; with several candidates and no common prefix, Tab / Shift+Tab cycle through them |
| `she pw` then Tab | the names of the kept sessions | same |
| Enter in the body | adds a line on a line with text; runs on an empty line or on `E` | Shift+Enter adds a line, Enter runs (PSReadLine) |

Other Tab, Shift+Tab and Enter work as usual. In PowerShell, `bash` (added by the installer) opens Git Bash in the same terminal; `exit` comes back.

## she on the host

`she-server.py` gives the same use inside an `ssh` login on the host, in place of tmux: it runs scripts there and sends input to the bashes that the PC's `she` keeps (only the PC's `she` makes them).

| Type | What it does |
|---|---|
| `she <<'E' … E` | runs in a fresh bash on the host |
| `she NAME <<'E'` / `she NAME <<< 'x'` | runs in, or sends input to, the bash the PC keeps for NAME |
| `she NAME` (alone) | sends one hidden line (a password) |
| `she -l` | the names |

Install it by linking `she-server.py` as `she` on the PATH (for example `~/bin/she`) and, for Tab, reading `server-completion.bash` from `~/.bashrc` (keep it next to `completion.bash`).

## Install

**Windows PC** (Windows 10/11, Git for Windows, an `ssh` host that takes your key without a password):

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File Install-She.ps1 -SshHost HOST
```

It puts `she.exe` (by default the one next to the script) in `%USERPROFILE%\bin` and on the user PATH, writes the Tab files to `%USERPROFILE%\.config\she\`, and adds lines to Git Bash's `~/.bashrc` and the PowerShell profile. Steps already done are skipped, so **run the same thing to update**. New terminals pick it up.

**Build** `she.exe` (on any machine with Go, Python 3 and bash): `bash build.sh` checks the sources and builds `she.exe` next to them.

**Host**: `bash` and `python3`. Nothing needs installing for the PC's `she`; the dispatcher is sent over the connection.

## How it works

```
she.exe (started for each call)
  └─ TCP on localhost + a token → helper (she.exe -daemon, started hidden by the first call;
                                   state in %USERPROFILE%\.she\state.json)
        └─ one SSH connection (golang.org/x/crypto/ssh; host, key and known_hosts from ssh -G)
              └─ dispatcher (python3 on the host, one per helper)
                    ├─ unnamed: bash --norc --noprofile -c for each call
                    ├─ named: one interactive bash with a pty for each name
                    └─ socket /run/user/<uid>/she/<pid>.sock (for she on the host; only this user can open it)
```

- **End of a named call**: the script is written to a file and sourced; its end is reported from `PROMPT_COMMAND` (run by bash before each prompt) as a marker with the call's number, exit code and prompt. A Ctrl+C that cuts the line still gives the marker; markers with an old number are dropped; the echo of the line typed in is removed from the output.
- **One helper at a time**: only the call that takes `%USERPROFILE%\.she\start.lock` starts it.
- **State outside AppData**: programs started from an MSIX-packaged app (such as the Claude desktop app) have their AppData writes redirected to the app's own folder, so a `she` started there and one started from a terminal would not see each other.
- The connection is started with `SHE=1` set, so a login script can tell it from a person's login.

## Files

| File | |
|---|---|
| `main.go` | she on the PC: calls, the helper, SSH |
| `modes.go` | the dispatcher, three ways (`SHE_MODE=channel` / `bash` / the default `agent`); `agentCode` is the Python run on the host |
| `completion.bash`, `completion.ps1` | Tab, embedded in `she.exe` (`she --bash-completion`, `she --powershell-completion`) |
| `she-server.py`, `server-completion.bash` | she on the host, and its Tab |
| `build.sh`, `Install-She.ps1` | build, install on a PC |

## Known limits

- Named sessions live only while the PC's helper runs (`she -stop`, a reboot or an update ends them).
- Input waits inside `sudo` are seen only when output stops for 1 s without a line end.
- In Windows PowerShell 5.1, words starting with `-` (`she -s`) are not completed (PowerShell takes them as its own parameters).
- In Git Bash, the first word on a `>` line after `cat <<X` is not completed (the same without she).
- Hidden input in mintty (the Git Bash window) goes through the bash `read -s` wrapper in `completion.bash`; it has not been tried there.

## License

MIT (`LICENSE`).
