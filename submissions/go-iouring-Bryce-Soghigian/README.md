# Go + raw io_uring + SQLite (cgo)

- **Language**: Go 1.27.2, no third-party modules
- **HTTP**: hand-rolled HTTP/1.1 (keep-alive, pipelining, Content-Length bodies) on a raw io_uring event loop
- **SQLite**: 3.54.0 amalgamation (pinned + sha256-checked), compiled by `build.sh` with
  profile-guided optimization and linked statically via a thin cgo layer
- **Nginx or direct**: direct (`HOST=0.0.0.0 PORT=80`). Works behind Nginx too.

## Design

- **One OS thread, one ring.** The main goroutine is locked to its thread. The ring is created with
  `SINGLE_ISSUER | DEFER_TASKRUN | COOP_TASKRUN`, so completion work runs only when we ask for events.
  `GOMAXPROCS=1` and `GOMEMLIMIT=512MiB` by default because the box has 1 vCPU / 2 GB.
- **Multishot accept + multishot recv into a provided buffer ring** (`IORING_REGISTER_PBUF_RING`):
  4096 × 4 KiB buffers are shared by all connections. The kernel picks a buffer only when data
  arrives, so 15k idle keep-alive connections cost no receive memory.
- **Batched sends**: responses are queued per connection and sent in one batch per loop pass.
- **SQLite inline on the loop thread**: one `NOMUTEX` connection with persistent prepared statements.
  Each row is read in a single cgo call. JSON is written by hand, with an 8-bytes-at-a-time
  escape scan. `GODEBUG=cgocheck=0` skips cgo pointer checks.
- **Group commit**: writes parsed during one loop pass run in one `BEGIN IMMEDIATE … COMMIT`
  (at most `WRITE_BATCH_MAX`=256 per transaction), and their responses go out only after the commit.
  A connection with a write pending stops parsing until the commit, so pipelined responses
  stay in order.
- **Bounded WAL**: WAL mode, `synchronous=NORMAL`, `wal_autocheckpoint=0`. A second connection
  runs a PASSIVE checkpoint every 200 ms. Above 32 MiB of WAL it uses RESTART so the WAL can rewind,
  and `journal_size_limit` shrinks the file back to 64 MiB. With PASSIVE alone, sustained writes
  grew the WAL to 2.7 GB in 15 s.
- **Pages from the OS**: `mmap_size`=1 GiB maps the whole database. SQLite's own cache is 16 MiB
  (`SQLITE_CACHE_KIB`); it is still needed for pages whose newest version lives in the WAL.
- **SQLite built for this workload** (`build.sh`): SQLite's recommended speed options
  (`DEFAULT_MEMSTATUS=0`, `OMIT_SHARED_CACHE`, `OMIT_DEPRECATED`, …), `-O2 -march=native` (it is built
  on the box), and **PGO**. Compile with `-fprofile-generate`, then run `sqlitebuild/train.c`. It
  builds a throwaway seed-like database and replays the server's exact statements, pragmas,
  group-commit batches and checkpoints in the load test's request mix. Then recompile with `-fprofile-use`.
  This adds ~30 s to the build. On `/feed`: Ubuntu's 3.45 gives 41.9k rps, our build 51.1k, our build + PGO 54.1k.
- **JSON rendered in Go, not SQL**: building it with SQLite's `json_object()` + `group_concat`
  (`SQL_JSON=1`) measured 32k vs 56k rps on `/feed` and was ~10% slower on `/posts/:id`.
- `SERVER_MODE=std` runs the same handlers on `net/http` for A/B comparison.

## Local numbers (Docker on Apple Silicon, server `--cpus=1 --memory=2g`, wrk in a separate container)

| | io_uring | net/http |
|---|---|---|
| `GET /feed`, 256 conns | 56k rps, p99 9 ms | 23.2k rps, p99 20 ms |
| `GET /posts/:id` (random), 256 conns | 142k rps, p99 5.5 ms | 57.3k rps, p99 8.9 ms |
| writes (85% like / 15% create), 256 conns | ~80k rps (41k unbatched) | 27.9k rps |
| `GET /feed`, 10k conns | 33.3k rps | 22.1k rps |
| process RSS after 10k-conn write run | 256 MiB (137 MiB anon) | |

These are arm64 results; the scoring droplet is x86_64. The container memory figure includes the
page cache for the 238 MB database copy. To reproduce:
`docker build -t twelve-go-uring -f local/Dockerfile . && docker build -t twelve-wrk -f local/Dockerfile.wrk local && bash local/bench.sh uring 256 15`.
The write mix needs `local/tokens.lua`, generated from `seed/tokens.json` (see `local/writes.lua`).
`local/memprobe.sh` breaks down memory, and `local/profile.sh` runs a `perf` CPU profile.
Docker's default seccomp profile blocks io_uring, so the server container runs with `seccomp=unconfined`.
