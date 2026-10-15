#!/usr/bin/env bash
# Runs the server in the foreground. Config: SQLITE_PATH, JWT_SECRET, HOST, PORT.
# One P and a soft heap cap: the box is 1 vCPU / 2 GB shared with the OS.
set -euo pipefail
# cgocheck=0: cgo pointer checks cost ~3% CPU on /feed; the C side never retains Go pointers.
export GODEBUG="${GODEBUG:-cgocheck=0}"
export GOMAXPROCS="${GOMAXPROCS:-1}" GOMEMLIMIT="${GOMEMLIMIT:-512MiB}" GOGC="${GOGC:-200}"
exec "$(dirname "$0")/bin/server"
