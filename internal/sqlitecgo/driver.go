//go:build cgo

// Package sqlitecgo links SQLite through the platform C library. Android builds
// require CGO_ENABLED=1 and an Android NDK compiler; see backend/README.md.
package sqlitecgo

import (
	"database/sql"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// Android's app seccomp policy rejects legacy Linux filesystem syscalls used
// by the pure-Go libc on some ABIs. Link SQLite to the NDK's Bionic libc so it
// uses the supported Android entry points. The database and SQL schema stay
// standard SQLite, including files previously written by the desktop driver.
func init() {
	sql.Register("sqlite", &sqlite3.SQLiteDriver{})
}
