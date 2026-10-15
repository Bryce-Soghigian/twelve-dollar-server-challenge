package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// writeOp is a validated, authenticated write waiting to run. Validation happens
// at parse time so only real writes reach the (group-committed) transaction.
type writeOp struct {
	like bool
	uid  int64
	id   int64 // like: target post; create: new post id
	name string
	text []byte

	// results, filled by execWrite
	err     bool
	res     int // like: 1 inserted, 0 already liked, -1 no such post
	created [32]byte
	nCreat  int
}

// prepareWrite reports whether path is a write route and, if so, validates it.
// A non-nil errBody means the request is answered without touching the DB.
func (a *app) prepareWrite(path, auth, body []byte) (op writeOp, isWrite bool, status int, errBody []byte) {
	path = stripQuery(path)
	switch {
	case string(path) == "/posts":
		uid, name, eb := a.auth(auth)
		if eb != nil {
			return op, true, 401, eb
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			// Valid JSON that isn't an object (e.g. `[]`) has no body field.
			if json.Valid(body) {
				return op, true, 400, errBodyRequired
			}
			return op, true, 400, errMalformed
		}
		s, ok := req["body"].(string)
		if !ok {
			return op, true, 400, errBodyRequired
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return op, true, 400, errBodyRequired
		}
		if utf8.RuneCountInString(s) > 500 {
			return op, true, 400, errBodyTooLong
		}
		return writeOp{uid: uid, name: name, text: []byte(s)}, true, 0, nil
	case bytes.HasPrefix(path, []byte("/posts/")) && bytes.HasSuffix(path, []byte("/like")):
		uid, _, eb := a.auth(auth)
		if eb != nil {
			return op, true, 401, eb
		}
		id, ok := parseID(path[len("/posts/") : len(path)-len("/like")])
		if !ok {
			return op, true, 400, errBadID
		}
		return writeOp{like: true, uid: uid, id: id}, true, 0, nil
	}
	return op, false, 0, nil
}

func (a *app) execWrite(op *writeOp) {
	if op.like {
		res, err := a.db.like(op.uid, op.id)
		op.res, op.err = res, err != nil
		return
	}
	id, created, err := a.db.insertPost(op.uid, op.text)
	op.id, op.err = id, err != nil
	op.nCreat = copy(op.created[:], created)
}

// execBatch runs ops in one transaction (group commit). If the transaction
// doesn't commit, every op in it fails.
func (a *app) execBatch(ops []writeOp) {
	fail := func() {
		for i := range ops {
			ops[i].err = true
		}
	}
	if err := a.db.begin(); err != nil {
		fail()
		return
	}
	for i := range ops {
		a.execWrite(&ops[i])
	}
	if err := a.db.commit(); err != nil {
		a.db.rollback()
		fail()
	}
}

func (a *app) renderWrite(op *writeOp) (int, []byte) {
	switch {
	case op.err:
		return 500, errInternal
	case op.like && op.res < 0:
		return 404, errNoPost
	case op.like:
		out := append(a.scratch[:0], `{"liked":true,"already_liked":`...)
		out = strconv.AppendBool(out, op.res == 0)
		out = append(out, `,"post_id":`...)
		out = strconv.AppendInt(out, op.id, 10)
		a.scratch = append(out, '}')
		if op.res == 1 {
			return 201, a.scratch
		}
		return 200, a.scratch
	}
	out := append(a.scratch[:0], `{"post":`...)
	out = appendPost(out, op.id, op.text, op.created[:op.nCreat], []byte(op.name), 0)
	a.scratch = append(out, '}')
	return 201, a.scratch
}
