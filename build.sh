#!/bin/bash
# build.sh - build she.exe for Windows and put it where the PCs fetch it
# (Install-She.ps1 copies ~/lab-tools/she/she.exe). Run on the server:
#   bash ~/lab-tools/she/build.sh
# Until 2026-10-12 it went to /usr/local/share/she to keep it out of the Drive
# backup, but /usr/local is backed up too: it stays next to its source.
set -euo pipefail
cd "$(dirname "$0")"
export PATH=$PATH:/usr/local/go/bin
gofmt -l . | grep . && { echo "gofmt: run gofmt -w on the files above" >&2; exit 1; }
# the dispatcher (Python, inside modes.go) must at least compile
awk '/^const agentCode = `/{f=1;sub(/^const agentCode = `/,"")} f{ if (/^`$/){exit} print }' modes.go > /tmp/she-agent-check.py
python3 -m py_compile /tmp/she-agent-check.py
bash -n completion.bash
bash -n server-completion.bash
python3 -m py_compile she-server.py
GOOS=windows GOARCH=amd64 go vet .
mkdir -p /tmp/she-build
GOOS=windows GOARCH=amd64 go build -trimpath -o /tmp/she-build/she.exe .
install -m 755 /tmp/she-build/she.exe she.exe
sha256sum she.exe
