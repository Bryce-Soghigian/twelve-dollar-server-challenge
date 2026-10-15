package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash"
	"strconv"
	"time"
)

// app holds per-thread request state. Not safe for concurrent use.
type app struct {
	db      *db
	mac     hash.Hash
	start   time.Time
	scratch []byte // response body under construction
	sig     [sha256.Size]byte
	dec     []byte
}

func newApp(d *db, secret string) *app {
	return &app{db: d, mac: hmac.New(sha256.New, []byte(secret)), start: time.Now(), scratch: make([]byte, 0, 16<<10)}
}

var (
	errNotFound     = []byte(`{"error":"not found"}`)
	errBadID        = []byte(`{"error":"invalid post id"}`)
	errNoPost       = []byte(`{"error":"post not found"}`)
	errNoBearer     = []byte(`{"error":"missing bearer token"}`)
	errBadToken     = []byte(`{"error":"invalid or expired token"}`)
	errBadPayload   = []byte(`{"error":"invalid token payload"}`)
	errMalformed    = []byte(`{"error":"malformed JSON body"}`)
	errBodyRequired = []byte(`{"error":"body is required"}`)
	errBodyTooLong  = []byte(`{"error":"body must be at most 500 characters"}`)
	errInternal     = []byte(`{"error":"internal server error"}`)
)

// handle routes one request and appends the complete HTTP response to out.
func (a *app) handle(out, method, path, auth, body []byte) []byte {
	status, b := a.route(method, stripQuery(path), auth, body)
	return appendResponse(out, status, b)
}

func stripQuery(path []byte) []byte {
	if i := bytes.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func (a *app) route(method, path, auth, body []byte) (int, []byte) {
	get := string(method) == "GET"
	post := string(method) == "POST"
	switch {
	case get && string(path) == "/feed" && a.db.feedJSON != nil:
		out, err := a.db.appendFeedJSON(a.scratch[:0])
		if err != nil {
			return 500, errInternal
		}
		a.scratch = out
		return 200, a.scratch
	case get && bytes.HasPrefix(path, []byte("/posts/")) && a.db.byIDJSON != nil:
		id, ok := parseID(path[len("/posts/"):])
		if !ok {
			return 400, errBadID
		}
		out, found, err := a.db.appendPostByIDJSON(a.scratch[:0], id)
		if err != nil {
			return 500, errInternal
		}
		if !found {
			return 404, errNoPost
		}
		a.scratch = out
		return 200, a.scratch
	case get && string(path) == "/feed":
		out := append(a.scratch[:0], `{"posts":[`...)
		out, err := a.db.appendFeed(out)
		if err != nil {
			return 500, errInternal
		}
		a.scratch = append(out, "]}"...)
		return 200, a.scratch
	case get && bytes.HasPrefix(path, []byte("/posts/")):
		id, ok := parseID(path[len("/posts/"):])
		if !ok {
			return 400, errBadID
		}
		out := append(a.scratch[:0], `{"post":`...)
		out, found, err := a.db.appendPostByID(out, id)
		if err != nil {
			return 500, errInternal
		}
		if !found {
			return 404, errNoPost
		}
		a.scratch = append(out, '}')
		return 200, a.scratch
	case get && string(path) == "/health":
		if err := a.db.health(); err != nil {
			out := append(a.scratch[:0], `{"status":"degraded","db":"unreachable","error":`...)
			a.scratch = append(appendJSONString(out, []byte(err.Error())), '}')
			return 503, a.scratch
		}
		out := append(a.scratch[:0], `{"status":"ok","db":"ok","uptime_s":`...)
		out = strconv.AppendInt(out, int64(time.Since(a.start)/time.Second), 10)
		a.scratch = append(out, '}')
		return 200, a.scratch
	case post:
		// Autocommit path (net/http mode); the io_uring loop batches writes instead.
		op, isWrite, st, eb := a.prepareWrite(path, auth, body)
		if !isWrite {
			break
		}
		if eb != nil {
			return st, eb
		}
		a.execWrite(&op)
		return a.renderWrite(&op)
	}
	return 404, errNotFound
}

// auth verifies an HS256 bearer token and returns (user id, username) or an error body.
func (a *app) auth(h []byte) (int64, string, []byte) {
	if !bytes.HasPrefix(h, []byte("Bearer ")) {
		return 0, "", errNoBearer
	}
	tok := h[len("Bearer "):]
	d1 := bytes.IndexByte(tok, '.')
	if d1 < 0 {
		return 0, "", errBadToken
	}
	d2 := bytes.IndexByte(tok[d1+1:], '.')
	if d2 < 0 {
		return 0, "", errBadToken
	}
	d2 += d1 + 1
	signed, sigPart := tok[:d2], tok[d2+1:]

	if base64.RawURLEncoding.DecodedLen(len(sigPart)) != sha256.Size {
		return 0, "", errBadToken
	}
	if _, err := base64.RawURLEncoding.Decode(a.sig[:], sigPart); err != nil {
		return 0, "", errBadToken
	}
	a.mac.Reset()
	a.mac.Write(signed)
	var sum [sha256.Size]byte
	if !hmac.Equal(a.mac.Sum(sum[:0]), a.sig[:]) {
		return 0, "", errBadToken
	}

	var hdr struct {
		Alg string `json:"alg"`
	}
	if !a.decodeSegment(tok[:d1], &hdr) || hdr.Alg != "HS256" {
		return 0, "", errBadToken
	}
	var claims struct {
		Sub      any `json:"sub"`
		Username any `json:"username"`
		Exp      any `json:"exp"`
	}
	if !a.decodeSegment(tok[d1+1:d2], &claims) {
		return 0, "", errBadToken
	}
	if exp, ok := claims.Exp.(float64); ok && float64(time.Now().Unix()) >= exp {
		return 0, "", errBadToken
	}
	sub, ok := claims.Sub.(string)
	if !ok {
		return 0, "", errBadPayload
	}
	uid, ok := parseID([]byte(sub))
	if !ok {
		return 0, "", errBadPayload
	}
	name, ok := claims.Username.(string)
	if !ok {
		return 0, "", errBadPayload
	}
	return uid, name, nil
}

func (a *app) decodeSegment(seg []byte, v any) bool {
	n := base64.RawURLEncoding.DecodedLen(len(seg))
	if cap(a.dec) < n {
		a.dec = make([]byte, n)
	}
	m, err := base64.RawURLEncoding.Decode(a.dec[:n], seg)
	if err != nil {
		return false
	}
	return json.Unmarshal(a.dec[:m], v) == nil
}

// parseID accepts a positive decimal integer.
func parseID(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n > 0
}

func appendPost(out []byte, id int64, body, created, author []byte, likes int64) []byte {
	out = append(out, `{"id":`...)
	out = strconv.AppendInt(out, id, 10)
	out = append(out, `,"body":`...)
	out = appendJSONString(out, body)
	out = append(out, `,"created_at":`...)
	out = appendJSONString(out, created)
	out = append(out, `,"author":`...)
	out = appendJSONString(out, author)
	out = append(out, `,"like_count":`...)
	out = strconv.AppendInt(out, likes, 10)
	return append(out, '}')
}

const hexDigits = "0123456789abcdef"

// hasSpecial reports whether any byte of x (8 bytes, SWAR) is < 0x20, '"' or '\\'.
func hasSpecial(x uint64) bool {
	const lo, hi = 0x0101010101010101, 0x8080808080808080
	ctl := (x - lo*0x20) &^ x & hi
	q := x ^ (lo * '"')
	b := x ^ (lo * '\\')
	return (ctl | ((q - lo) &^ q & hi) | ((b - lo) &^ b & hi)) != 0
}

func needsEscape(c byte) bool { return c < 0x20 || c == '"' || c == '\\' }

func appendJSONString(out, s []byte) []byte {
	out = append(out, '"')
	for len(s) > 0 {
		i := 0
		for i+8 <= len(s) && !hasSpecial(binary.LittleEndian.Uint64(s[i:])) {
			i += 8
		}
		for i < len(s) && !needsEscape(s[i]) {
			i++
		}
		out = append(out, s[:i]...)
		if i == len(s) {
			break
		}
		switch c := s[i]; c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		s = s[i+1:]
	}
	return append(out, '"')
}

var statusLines = map[int]string{
	200: "HTTP/1.1 200 OK\r\n",
	201: "HTTP/1.1 201 Created\r\n",
	400: "HTTP/1.1 400 Bad Request\r\n",
	401: "HTTP/1.1 401 Unauthorized\r\n",
	404: "HTTP/1.1 404 Not Found\r\n",
	500: "HTTP/1.1 500 Internal Server Error\r\n",
	503: "HTTP/1.1 503 Service Unavailable\r\n",
}

func appendResponse(out []byte, status int, body []byte) []byte {
	out = append(out, statusLines[status]...)
	out = append(out, "Content-Type: application/json\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\n\r\n"...)
	return append(out, body...)
}
