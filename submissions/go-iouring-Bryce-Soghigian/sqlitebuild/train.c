// PGO training run for the vendored SQLite. build.sh compiles SQLite with
// -fprofile-generate, links it into this program, runs it, then recompiles
// SQLite with -fprofile-use.
//
// It builds a throwaway database with the challenge schema and seed-like data,
// then replays the server's exact statements and settings in the load test's
// mix: per simulated user, GET /feed, GET /posts/:id, a 15% like and a 2%
// create. Writes are group-committed in BEGIN IMMEDIATE batches, as the
// io_uring loop does. Checkpoints run the way checkpointLoop runs them.
//
//   train <db-path> [requests]
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include "sqlite3.h"

#define USERS 20000
#define POSTS 100000
#define LIKES 400000

static sqlite3 *db;

static void die(const char *what) {
	fprintf(stderr, "train: %s: %s\n", what, sqlite3_errmsg(db));
	exit(1);
}

static void exec(const char *sql) {
	if (sqlite3_exec(db, sql, 0, 0, 0) != SQLITE_OK) die(sql);
}

static sqlite3_stmt *prep(const char *sql) {
	sqlite3_stmt *s;
	if (sqlite3_prepare_v3(db, sql, -1, SQLITE_PREPARE_PERSISTENT, &s, 0) != SQLITE_OK) die(sql);
	return s;
}

static unsigned long long rng = 88172645463325252ULL;
static unsigned rnd(void) {
	rng ^= rng << 13; rng ^= rng >> 7; rng ^= rng << 17;
	return (unsigned)(rng >> 11);
}
static double frand(void) { return (rnd() & 0xffffff) / (double)0x1000000; }

static const char *words[] = {
	"server", "database", "request", "latency", "cache", "index", "query", "benchmark", "droplet",
	"cpu", "memory", "disk", "network", "throughput", "users", "coffee", "deploy", "release", "bug",
	"feature", "the", "a", "and", "really", "very", "today", "kernel", "socket", "keepalive", "pool",
};

static int body(char *out, int max) {
	int n = 0, target = 30 + rnd() % 170;
	while (n < target && n < max - 16) {
		const char *w = words[rnd() % (sizeof words / sizeof *words)];
		size_t l = strlen(w);
		memcpy(out + n, w, l);
		n += l;
		out[n++] = ' ';
	}
	out[n - 1] = '.';
	return n;
}

// Reads every column, like step_post in db.go.
static int drain_posts(sqlite3_stmt *s) {
	int rows = 0, rc;
	volatile long long sink = 0;
	while ((rc = sqlite3_step(s)) == SQLITE_ROW) {
		sink += sqlite3_column_int64(s, 0);
		sink += sqlite3_column_bytes(s, 1) + (long long)(size_t)sqlite3_column_text(s, 1);
		sink += sqlite3_column_bytes(s, 2) + (long long)(size_t)sqlite3_column_text(s, 2);
		sink += sqlite3_column_bytes(s, 3) + (long long)(size_t)sqlite3_column_text(s, 3);
		sink += sqlite3_column_int64(s, 4);
		rows++;
	}
	if (rc != SQLITE_DONE) die("step post");
	sqlite3_reset(s);
	return rows;
}

int main(int argc, char **argv) {
	if (argc < 2) { fprintf(stderr, "usage: train <db-path> [requests]\n"); return 2; }
	const char *path = argv[1];
	long requests = argc > 2 ? atol(argv[2]) : 150000;
	char p[512];
	const char *sufs[] = {"", "-wal", "-shm"};
	for (int i = 0; i < 3; i++) {
		snprintf(p, sizeof p, "%s%s", path, sufs[i]);
		unlink(p);
	}

	if (sqlite3_open_v2(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_NOMUTEX, 0) != SQLITE_OK) die("open");

	// Same schema as ../../schema.sql (copied so the build is self-contained).
	exec("PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF;"
	     "CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT NOT NULL UNIQUE,"
	     "  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')));"
	     "CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),"
	     "  body TEXT NOT NULL CHECK (length(body) BETWEEN 1 AND 500),"
	     "  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')));"
	     "CREATE TABLE likes (user_id INTEGER NOT NULL REFERENCES users(id), post_id INTEGER NOT NULL REFERENCES posts(id),"
	     "  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), PRIMARY KEY (user_id, post_id));"
	     "CREATE INDEX posts_created_at_id_idx ON posts (created_at DESC, id DESC);"
	     "CREATE INDEX posts_user_id_idx ON posts (user_id);"
	     "CREATE INDEX likes_post_id_idx ON likes (post_id);");

	exec("BEGIN");
	sqlite3_stmt *iu = prep("INSERT INTO users (id, username, created_at) VALUES (?, ?, '2025-01-01T00:00:00.000Z')");
	for (int i = 1; i <= USERS; i++) {
		char name[48];
		int n = snprintf(name, sizeof name, "%s_%s_%d", words[rnd() % 30], words[rnd() % 30], i);
		sqlite3_bind_int(iu, 1, i);
		sqlite3_bind_text(iu, 2, name, n, SQLITE_TRANSIENT);
		if (sqlite3_step(iu) != SQLITE_DONE) die("seed users");
		sqlite3_reset(iu);
	}
	sqlite3_stmt *ip = prep("INSERT INTO posts (id, user_id, body, created_at) VALUES (?, ?, ?, ?)");
	for (int i = 1; i <= POSTS; i++) {
		char b[600], ts[32];
		long ms = (long)i * 315000L; // spread across the year, increasing like the seed
		snprintf(ts, sizeof ts, "2025-%02ld-%02ldT%02ld:%02ld:%02ld.%03ldZ",
		         1 + (ms / 2592000000L) % 12, 1 + (ms / 86400000L) % 28, (ms / 3600000L) % 24,
		         (ms / 60000L) % 60, (ms / 1000L) % 60, ms % 1000);
		sqlite3_bind_int(ip, 1, i);
		sqlite3_bind_int(ip, 2, 1 + rnd() % USERS);
		sqlite3_bind_text(ip, 3, b, body(b, sizeof b), SQLITE_TRANSIENT);
		sqlite3_bind_text(ip, 4, ts, -1, SQLITE_TRANSIENT);
		if (sqlite3_step(ip) != SQLITE_DONE) die("seed posts");
		sqlite3_reset(ip);
	}
	sqlite3_stmt *il = prep("INSERT OR IGNORE INTO likes (user_id, post_id) VALUES (?, ?)");
	for (int i = 0; i < LIKES; i++) {
		sqlite3_bind_int(il, 1, 1 + rnd() % USERS);
		sqlite3_bind_int(il, 2, 1 + rnd() % POSTS);
		if (sqlite3_step(il) != SQLITE_DONE) die("seed likes");
		sqlite3_reset(il);
	}
	exec("COMMIT; ANALYZE; PRAGMA journal_mode=WAL;");
	sqlite3_finalize(iu); sqlite3_finalize(ip); sqlite3_finalize(il);

	// The server's pragmas (db.go openDB).
	exec("PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000; PRAGMA cache_size=-16384;"
	     "PRAGMA mmap_size=1073741824; PRAGMA temp_store=MEMORY; PRAGMA wal_autocheckpoint=0;"
	     "PRAGMA journal_size_limit=67108864;");

	// The server's statements (db.go), verbatim.
#define POST_COLS "SELECT p.id, p.body, p.created_at, u.username,\n" \
	"       (SELECT count(*) FROM likes l WHERE l.post_id = p.id)\n" \
	"  FROM posts p JOIN users u ON u.id = p.user_id "
	sqlite3_stmt *feed = prep(POST_COLS "ORDER BY p.created_at DESC, p.id DESC LIMIT 20");
	sqlite3_stmt *byid = prep(POST_COLS "WHERE p.id = ?");
	sqlite3_stmt *ins = prep("INSERT INTO posts (user_id, body) VALUES (?, ?) RETURNING id, created_at");
	sqlite3_stmt *like = prep("INSERT INTO likes (user_id, post_id) SELECT ?1, id FROM posts WHERE id = ?2 ON CONFLICT (user_id, post_id) DO NOTHING");
	sqlite3_stmt *exists = prep("SELECT 1 FROM posts WHERE id = ?");
	sqlite3_stmt *ping = prep("SELECT 1");
	sqlite3_stmt *begin = prep("BEGIN IMMEDIATE"), *commit = prep("COMMIT");

	enum { BATCH = 64 };
	struct { int like; int uid; long long pid; } q[BATCH];
	int nq = 0;
	long long maxid = POSTS;
	for (long r = 0; r < requests; r++) {
		drain_posts(feed);
		long long pid = (rnd() & 3) ? maxid - rnd() % 20 : 1 + rnd() % maxid; // mostly from the feed
		if (rnd() % 64 == 0) pid = maxid + 1000 + rnd() % 1000;              // a 404 now and then
		sqlite3_bind_int64(byid, 1, pid);
		drain_posts(byid);
		int uid = 1 + rnd() % USERS;
		if (frand() < 0.15) q[nq].like = 1, q[nq].uid = uid, q[nq++].pid = pid;
		if (nq < BATCH && frand() < 0.02) q[nq].like = 0, q[nq].uid = uid, q[nq++].pid = 0;
		if (r % 500 == 0) { sqlite3_step(ping); sqlite3_reset(ping); }

		// Flush a group commit every few dozen requests, as a loop pass would.
		if (nq == BATCH || (nq > 0 && r % 24 == 0)) {
			if (sqlite3_step(begin) != SQLITE_DONE) die("begin");
			sqlite3_reset(begin);
			for (int i = 0; i < nq; i++) {
				if (q[i].like) {
					sqlite3_bind_int64(like, 1, q[i].uid);
					sqlite3_bind_int64(like, 2, q[i].pid);
					if (sqlite3_step(like) != SQLITE_DONE) die("like");
					sqlite3_reset(like);
					if (sqlite3_changes(db) == 0) {
						sqlite3_bind_int64(exists, 1, q[i].pid);
						sqlite3_step(exists);
						sqlite3_reset(exists);
					}
				} else {
					char b[600];
					sqlite3_bind_int64(ins, 1, q[i].uid);
					sqlite3_bind_text(ins, 2, b, body(b, sizeof b), SQLITE_STATIC);
					if (sqlite3_step(ins) != SQLITE_ROW) die("insert post");
					maxid = sqlite3_column_int64(ins, 0);
					(void)sqlite3_column_text(ins, 1);
					sqlite3_step(ins);
					sqlite3_reset(ins);
					sqlite3_clear_bindings(ins);
				}
			}
			if (sqlite3_step(commit) != SQLITE_DONE) die("commit");
			sqlite3_reset(commit);
			nq = 0;
		}
		// checkpointLoop: PASSIVE often, RESTART when the WAL gets big.
		if (r % 2000 == 0) {
			int nlog = 0, nckpt = 0;
			sqlite3_wal_checkpoint_v2(db, 0, SQLITE_CHECKPOINT_PASSIVE, &nlog, &nckpt);
			if (nlog > 8192) sqlite3_wal_checkpoint_v2(db, 0, SQLITE_CHECKPOINT_RESTART, &nlog, &nckpt);
		}
	}
	sqlite3_finalize(feed); sqlite3_finalize(byid); sqlite3_finalize(ins); sqlite3_finalize(like);
	sqlite3_finalize(exists); sqlite3_finalize(ping); sqlite3_finalize(begin); sqlite3_finalize(commit);
	if (sqlite3_close(db) != SQLITE_OK) die("close");
	fprintf(stderr, "train: %ld requests, last post id %lld\n", requests, maxid);
	return 0;
}
