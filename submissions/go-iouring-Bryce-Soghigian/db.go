package main

/*
#include <sqlite3.h>
#include <stdlib.h>

typedef struct {
	sqlite3_int64 id;
	sqlite3_int64 likes;
	const unsigned char *body, *created, *author;
	int body_n, created_n, author_n;
} post_row;

// One cgo crossing per row instead of eight.
static int step_post(sqlite3_stmt *s, post_row *r) {
	int rc = sqlite3_step(s);
	if (rc != SQLITE_ROW) return rc;
	r->id = sqlite3_column_int64(s, 0);
	r->body = sqlite3_column_text(s, 1);    r->body_n = sqlite3_column_bytes(s, 1);
	r->created = sqlite3_column_text(s, 2); r->created_n = sqlite3_column_bytes(s, 2);
	r->author = sqlite3_column_text(s, 3);  r->author_n = sqlite3_column_bytes(s, 3);
	r->likes = sqlite3_column_int64(s, 4);
	return rc;
}

// Single-text-column result (JSON rendered by SQLite): one crossing per request.
static int step_text(sqlite3_stmt *s, const unsigned char **p, int *n) {
	int rc = sqlite3_step(s);
	if (rc == SQLITE_ROW) { *p = sqlite3_column_text(s, 0); *n = sqlite3_column_bytes(s, 0); }
	return rc;
}

static int bind_text(sqlite3_stmt *s, int i, const char *p, int n) {
	return sqlite3_bind_text(s, i, p, n, SQLITE_TRANSIENT);
}

// INSERT ... RETURNING id, created_at; copies created_at into out (24 bytes).
static int insert_post(sqlite3_stmt *s, sqlite3_int64 uid, const char *body, int n, sqlite3_int64 *id, char *out, int *out_n) {
	sqlite3_bind_int64(s, 1, uid);
	sqlite3_bind_text(s, 2, body, n, SQLITE_STATIC);
	int rc = sqlite3_step(s);
	if (rc == SQLITE_ROW) {
		*id = sqlite3_column_int64(s, 0);
		int cn = sqlite3_column_bytes(s, 1);
		if (cn > 32) cn = 32;
		const unsigned char *c = sqlite3_column_text(s, 1);
		for (int i = 0; i < cn; i++) out[i] = c[i];
		*out_n = cn;
		rc = sqlite3_step(s); // drain to SQLITE_DONE so the statement commits
	}
	sqlite3_reset(s);
	sqlite3_clear_bindings(s);
	return rc;
}

// Returns 1 = inserted, 0 = already liked, -1 = no such post, <-1 = -sqlite error.
static int like_post(sqlite3 *db, sqlite3_stmt *ins, sqlite3_stmt *exists, sqlite3_int64 uid, sqlite3_int64 pid) {
	sqlite3_bind_int64(ins, 1, uid);
	sqlite3_bind_int64(ins, 2, pid);
	int rc = sqlite3_step(ins);
	sqlite3_reset(ins);
	if (rc != SQLITE_DONE) return -1000 - rc;
	if (sqlite3_changes(db) == 1) return 1;
	sqlite3_bind_int64(exists, 1, pid);
	rc = sqlite3_step(exists);
	sqlite3_reset(exists);
	if (rc == SQLITE_ROW) return 0;
	if (rc == SQLITE_DONE) return -1;
	return -1000 - rc;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// JSON built inside SQLite (SQL_JSON=1), for A/B against rendering in Go.
const postJSON = `json_object('id', p.id, 'body', p.body, 'created_at', p.created_at, 'author', u.username,
       'like_count', (SELECT count(*) FROM likes l WHERE l.post_id = p.id))`

const postCols = `SELECT p.id, p.body, p.created_at, u.username,
       (SELECT count(*) FROM likes l WHERE l.post_id = p.id)
  FROM posts p JOIN users u ON u.id = p.user_id `

// db owns one SQLite connection and its prepared statements. It is not safe for
// concurrent use: the io_uring loop calls it from its single locked OS thread.
type db struct {
	conn                                       *C.sqlite3
	feed, byID, insPost, insLike, exists, ping *C.sqlite3_stmt
	begin_, commit_, rollback_                 *C.sqlite3_stmt
	row                                        C.post_row
	feedJSON, byIDJSON                         *C.sqlite3_stmt // non-nil when SQL_JSON=1
}

func openDB(path string, nomutex bool) (*db, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	flags := C.int(C.SQLITE_OPEN_READWRITE)
	if nomutex {
		flags |= C.SQLITE_OPEN_NOMUTEX
	} else {
		flags |= C.SQLITE_OPEN_FULLMUTEX
	}
	d := &db{}
	if rc := C.sqlite3_open_v2(cpath, &d.conn, flags, nil); rc != C.SQLITE_OK {
		return nil, fmt.Errorf("open %s: %s", path, C.GoString(C.sqlite3_errstr(rc)))
	}
	// journal_mode doesn't fail when it can't switch; it returns the mode it ended up in.
	if mode, err := d.queryText("PRAGMA journal_mode=WAL"); err != nil || mode != "wal" {
		return nil, fmt.Errorf("journal_mode=WAL: got %q (%v)", mode, err)
	}
	for _, p := range []string{
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA cache_size=-" + env("SQLITE_CACHE_KIB", "16384"),
		"PRAGMA mmap_size=" + env("SQLITE_MMAP_BYTES", "1073741824"),
		"PRAGMA temp_store=MEMORY",
		"PRAGMA wal_autocheckpoint=0",        // checkpoints run off the event loop, see checkpointLoop
		"PRAGMA journal_size_limit=67108864", // shrink the WAL file back after it rewinds
	} {
		if err := d.exec(p); err != nil {
			return nil, err
		}
	}
	var err error
	prep := func(sql string) *C.sqlite3_stmt {
		if err != nil {
			return nil
		}
		var s *C.sqlite3_stmt
		cs := C.CString(sql)
		defer C.free(unsafe.Pointer(cs))
		if rc := C.sqlite3_prepare_v3(d.conn, cs, -1, C.SQLITE_PREPARE_PERSISTENT, &s, nil); rc != C.SQLITE_OK {
			err = fmt.Errorf("prepare %q: %s", sql, d.errmsg())
		}
		return s
	}
	d.feed = prep(postCols + "ORDER BY p.created_at DESC, p.id DESC LIMIT 20")
	d.byID = prep(postCols + "WHERE p.id = ?")
	d.insPost = prep("INSERT INTO posts (user_id, body) VALUES (?, ?) RETURNING id, created_at")
	d.insLike = prep("INSERT INTO likes (user_id, post_id) SELECT ?1, id FROM posts WHERE id = ?2 ON CONFLICT (user_id, post_id) DO NOTHING")
	d.exists = prep("SELECT 1 FROM posts WHERE id = ?")
	d.ping = prep("SELECT 1")
	if env("SQL_JSON", "0") == "1" {
		d.feedJSON = prep(`SELECT '{"posts":[' || coalesce(group_concat(j, ',' ORDER BY rn), '') || ']}' FROM (
  SELECT ` + postJSON + ` AS j, row_number() OVER (ORDER BY p.created_at DESC, p.id DESC) AS rn
    FROM posts p JOIN users u ON u.id = p.user_id ORDER BY p.created_at DESC, p.id DESC LIMIT 20)`)
		d.byIDJSON = prep(`SELECT '{"post":' || ` + postJSON + ` || '}' FROM posts p JOIN users u ON u.id = p.user_id WHERE p.id = ?`)
	}
	d.begin_ = prep("BEGIN IMMEDIATE")
	d.commit_ = prep("COMMIT")
	d.rollback_ = prep("ROLLBACK")
	return d, err
}

func (d *db) errmsg() string { return C.GoString(C.sqlite3_errmsg(d.conn)) }

func (d *db) exec(sql string) error {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	if rc := C.sqlite3_exec(d.conn, cs, nil, nil, nil); rc != C.SQLITE_OK {
		return fmt.Errorf("%s: %s", sql, d.errmsg())
	}
	return nil
}

func (d *db) run(s *C.sqlite3_stmt) error {
	rc := C.sqlite3_step(s)
	C.sqlite3_reset(s)
	if rc != C.SQLITE_DONE {
		return errors.New(d.errmsg())
	}
	return nil
}

func (d *db) begin() error { return d.run(d.begin_) }

// commit fails if an error inside the transaction already rolled it back.
func (d *db) commit() error {
	if C.sqlite3_get_autocommit(d.conn) != 0 {
		return errors.New("transaction was rolled back")
	}
	return d.run(d.commit_)
}

func (d *db) rollback() {
	if C.sqlite3_get_autocommit(d.conn) == 0 {
		d.run(d.rollback_)
	}
}

// checkpoint copies WAL frames into the database file and returns how many
// frames the WAL holds and how many were backfilled.
func (d *db) queryText(sql string) (string, error) {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	var s *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(d.conn, cs, -1, &s, nil); rc != C.SQLITE_OK {
		return "", errors.New(d.errmsg())
	}
	defer C.sqlite3_finalize(s)
	if C.sqlite3_step(s) != C.SQLITE_ROW {
		return "", errors.New(d.errmsg())
	}
	return C.GoString((*C.char)(unsafe.Pointer(C.sqlite3_column_text(s, 0)))), nil
}

func (d *db) checkpoint(restart bool) (log, done int, err error) {
	mode := C.int(C.SQLITE_CHECKPOINT_PASSIVE)
	if restart {
		mode = C.SQLITE_CHECKPOINT_RESTART
	}
	var nLog, nCkpt C.int
	if rc := C.sqlite3_wal_checkpoint_v2(d.conn, nil, mode, &nLog, &nCkpt); rc != C.SQLITE_OK && rc != C.SQLITE_BUSY {
		return 0, 0, errors.New(d.errmsg())
	}
	return int(nLog), int(nCkpt), nil
}

func (d *db) health() error {
	rc := C.sqlite3_step(d.ping)
	C.sqlite3_reset(d.ping)
	if rc != C.SQLITE_ROW {
		return errors.New(d.errmsg())
	}
	return nil
}

func cbytes(p *C.uchar, n C.int) []byte {
	if n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n))
}

// appendPosts renders every row of s as a post object, comma separated.
// Returns the number of rows written.
func (d *db) appendPosts(out []byte, s *C.sqlite3_stmt) ([]byte, int, error) {
	n := 0
	for {
		rc := C.step_post(s, &d.row)
		if rc == C.SQLITE_DONE {
			break
		}
		if rc != C.SQLITE_ROW {
			C.sqlite3_reset(s)
			return out, n, errors.New(d.errmsg())
		}
		if n > 0 {
			out = append(out, ',')
		}
		r := &d.row
		out = appendPost(out, int64(r.id), cbytes(r.body, r.body_n), cbytes(r.created, r.created_n), cbytes(r.author, r.author_n), int64(r.likes))
		n++
	}
	C.sqlite3_reset(s)
	return out, n, nil
}

// appendText appends the single text column of s's first row; found=false if no row.
func (d *db) appendText(out []byte, s *C.sqlite3_stmt) ([]byte, bool, error) {
	var p *C.uchar
	var n C.int
	rc := C.step_text(s, &p, &n)
	if rc == C.SQLITE_ROW {
		out = append(out, cbytes(p, n)...)
	}
	C.sqlite3_reset(s)
	if rc != C.SQLITE_ROW && rc != C.SQLITE_DONE {
		return out, false, errors.New(d.errmsg())
	}
	return out, rc == C.SQLITE_ROW, nil
}

func (d *db) appendFeedJSON(out []byte) ([]byte, error) {
	out, _, err := d.appendText(out, d.feedJSON)
	return out, err
}

func (d *db) appendPostByIDJSON(out []byte, id int64) ([]byte, bool, error) {
	C.sqlite3_bind_int64(d.byIDJSON, 1, C.sqlite3_int64(id))
	return d.appendText(out, d.byIDJSON)
}

func (d *db) appendFeed(out []byte) ([]byte, error) {
	out, _, err := d.appendPosts(out, d.feed)
	return out, err
}

// appendPostByID returns found=false if there is no such post.
func (d *db) appendPostByID(out []byte, id int64) ([]byte, bool, error) {
	C.sqlite3_bind_int64(d.byID, 1, C.sqlite3_int64(id))
	out, n, err := d.appendPosts(out, d.byID)
	return out, n == 1, err
}

func (d *db) insertPost(uid int64, body []byte) (id int64, created []byte, err error) {
	var cid C.sqlite3_int64
	var buf [32]C.char
	var n C.int
	rc := C.insert_post(d.insPost, C.sqlite3_int64(uid), (*C.char)(unsafe.Pointer(&body[0])), C.int(len(body)), &cid, &buf[0], &n)
	if rc != C.SQLITE_DONE {
		return 0, nil, errors.New(d.errmsg())
	}
	return int64(cid), C.GoBytes(unsafe.Pointer(&buf[0]), n), nil
}

// like returns 1 inserted, 0 already liked, -1 no such post.
func (d *db) like(uid, pid int64) (int, error) {
	rc := C.like_post(d.conn, d.insLike, d.exists, C.sqlite3_int64(uid), C.sqlite3_int64(pid))
	if rc < -1 {
		return 0, errors.New(d.errmsg())
	}
	return int(rc), nil
}
