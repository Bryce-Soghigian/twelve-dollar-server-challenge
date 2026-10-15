# Local stand-in for the $12 droplet: run with --cpus=1 --memory=2g.
# io_uring needs --security-opt seccomp=unconfined under Docker's default profile.
FROM ubuntu:24.04
COPY install.sh /tmp/install.sh
RUN bash /tmp/install.sh && apt-get install -y --no-install-recommends jq openssl sqlite3 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY go.mod *.go build.sh start.sh ./
COPY sqlitebuild ./sqlitebuild
# Variants for A/B: --build-arg SQLITE_PGO=0, --build-arg GO_TAGS=systemsqlite
ARG SQLITE_PGO=1
ARG GO_TAGS=
RUN SQLITE_PGO=$SQLITE_PGO GO_TAGS=$GO_TAGS bash build.sh
