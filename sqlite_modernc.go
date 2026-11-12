//go:build !android && !onit_sqlite_native

package main

import _ "modernc.org/sqlite"

const sqliteOptions = "?_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
