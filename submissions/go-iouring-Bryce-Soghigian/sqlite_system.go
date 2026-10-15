//go:build systemsqlite

package main

// Ubuntu's libsqlite3 (apt libsqlite3-dev), for A/B against the vendored build.

// #cgo LDFLAGS: -lsqlite3
import "C"
