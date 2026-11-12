package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Both drivers must apply safety options on the actual connection. Their DSN
// option names differ, and silently ignoring one would change stored-data rules.
func TestSQLiteConnectionContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contract.sqlite")
	first, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for pragma, want := range map[string]int{"foreign_keys": 1, "secure_delete": 1, "busy_timeout": 5000} {
		var got int
		if err := first.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: got %d, want %d, err %v", pragma, got, want, err)
		}
	}
	if _, err := first.db.Exec(`INSERT INTO sources (id,user_id,title,kind,body,digest,created_at)
        VALUES ('orphan','missing-user','test','self','text',X'01','now')`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key constraint") {
		t.Fatalf("expected foreign key rejection, got %v", err)
	}

	second, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// A short timeout keeps this invariant test quick. The default was checked
	// above. An immediate transaction must hold the writer reservation at Begin,
	// before either transaction has issued a write.
	if _, err := second.db.Exec("PRAGMA busy_timeout=10"); err != nil {
		t.Fatal(err)
	}
	tx, err := first.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	other, err := second.db.Begin()
	if err == nil {
		other.Rollback()
		t.Fatal("a second writer began before the immediate transaction ended")
	}
}
