#!/usr/bin/env bash
# Runs once as root on a clean Ubuntu 24.04: Go toolchain + gcc (SQLite is built from source by build.sh).
# libsqlite3-dev is only for the `systemsqlite` A/B build tag.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends build-essential curl ca-certificates libsqlite3-dev
ARCH="$(dpkg --print-architecture)"
rm -rf /usr/local/go
curl -fsSL "https://go.dev/dl/go1.27.2.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz
/usr/local/go/bin/go version
