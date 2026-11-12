package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A saved system copy must survive a real SQLite close/reopen as preparation
// state, but a later withdrawal must not turn it back into user text or show
// the withdrawn material.
func TestPreparedSystemCopySurvivesSQLiteReopenWithoutLaundering(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "prepared.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		s.Close()
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	summary := strings.TrimSpace(str(prep, "computedSummary"))
	prep["text"] = summary
	prep["textMode"] = "system"
	prep["systemCopy"] = summary
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["plan"] = object{"text": summary, "textMode": "system", "systemCopy": summary}
	state["artifacts"] = toAnyObjects(artifacts)

	if r := bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state}); r["revision"] != 1 {
		s.Close()
		t.Fatalf("write: %v", r)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := bridgeRequest(t, s, object{"op": "read"})
	restored := r["state"].(object)
	restoredPrep := objectOrEmpty(objectsOrEmpty(restored["goals"])[0]["preparation"])
	if str(restoredPrep, "textMode") != "system" || str(restoredPrep, "systemCopy") != summary {
		t.Fatalf("preparation copy did not survive reopen: %s", encode(restoredPrep))
	}

	// Withdraw the material after the reopen. The next local tick must not
	// treat the stored copy as the user's own words.
	restored["facts"] = []any{}
	made, changed, err := RunProactive(restored, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("withdraw made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "合成材料") {
		t.Fatalf("withdrawn material survived reopen: %s", xml)
	}
	if strings.Contains(xml, `<result kind="user">`) {
		t.Fatalf("stored system copy was laundered into user text: %s", xml)
	}
	restoredPrep = objectOrEmpty(objectsOrEmpty(restored["goals"])[0]["preparation"])
	if _, ok := restoredPrep["text"]; ok {
		t.Fatalf("preparation text survived withdrawal: %s", encode(restoredPrep))
	}
}
