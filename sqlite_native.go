//go:build android || onit_sqlite_native

package main

// This package requires cgo. Keeping that constraint at the package boundary
// also makes CGO_ENABLED=0 fail at build time instead of creating a broken APK.
import _ "github.com/ninondev/onit-core/internal/sqlitecgo"

const sqliteOptions = "?_foreign_keys=on&_secure_delete=on&_busy_timeout=5000&_txlock=immediate"
