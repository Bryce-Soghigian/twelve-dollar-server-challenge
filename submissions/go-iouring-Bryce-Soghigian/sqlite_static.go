//go:build !systemsqlite

package main

// Vendored SQLite, built (with PGO) by build.sh into build/libsqlite3.a.

// #cgo CFLAGS: -I${SRCDIR}/build
// #cgo LDFLAGS: ${SRCDIR}/build/libsqlite3.a -lm
import "C"
