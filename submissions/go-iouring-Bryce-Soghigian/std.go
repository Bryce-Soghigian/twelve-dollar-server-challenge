//go:build linux

package main

import (
	"io"
	"net/http"
	"sync"
)

// runStd is the net/http baseline for A/B comparison (SERVER_MODE=std). Same
// handlers and SQLite layer, serialized behind a mutex onto one connection.
func runStd(addr, path, secret string) error {
	d, err := openDB(path, false)
	if err != nil {
		return err
	}
	a := newApp(d, secret)
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		status, b := a.route([]byte(r.Method), []byte(r.URL.Path), []byte(r.Header.Get("Authorization")), body)
		w.Header()["Content-Type"] = []string{"application/json"}
		w.WriteHeader(status)
		w.Write(b)
		mu.Unlock()
	})
	return http.ListenAndServe(addr, h)
}
