package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A legacy preparation copy with no provenance must not be promoted to user
// text on the next local tick. It is kept aside as a recoverable copy and the
// rebuilt card must not render it.
func TestPreparedLegacyCopyQuarantinedOnNextTick(t *testing.T) {
	secret := "合成旧系统私密标记甲"
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["text"] = secret
	delete(prep, "textMode")
	delete(prep, "userNote")
	delete(prep, "systemCopy")
	delete(prep, "summaryHistory")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["plan"] = object{"text": secret}
	state["artifacts"] = toAnyObjects(artifacts)

	// Withdraw the cited material, then run the real next tick.
	state["facts"] = []any{}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("tick made=%d changed=%v err=%v", len(made), changed, err)
	}
	artifact := objectsOrEmpty(state["artifacts"])[0]
	plan := objectOrEmpty(artifact["plan"])
	if str(plan, "text") != "" {
		t.Fatalf("legacy plan text was promoted: %s", encode(plan))
	}
	if str(plan, "legacyText") != secret || plan["textNeedsReview"] != true {
		t.Fatalf("legacy plan copy was not quarantined: %s", encode(plan))
	}
	prep = objectOrEmpty(objectsOrEmpty(state["goals"])[0]["preparation"])
	if str(prep, "text") != "" {
		t.Fatalf("legacy preparation text was promoted: %s", encode(prep))
	}
	if str(prep, "legacyText") != secret || prep["textNeedsReview"] != true {
		t.Fatalf("legacy preparation copy was not quarantined: %s", encode(prep))
	}
	if strings.Contains(str(artifact, "xml"), secret) {
		t.Fatalf("rebuilt card rendered the legacy copy: %s", str(artifact, "xml"))
	}
}

// The quarantine must survive a real SQLite close/reopen, and a second reopen
// after the withdrawal tick must still keep the recoverable copy aside.
func TestPreparedLegacyCopySurvivesSQLiteReopenAsReview(t *testing.T) {
	secret := "合成旧系统私密标记甲"
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
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
	prep["text"] = secret
	delete(prep, "textMode")
	delete(prep, "userNote")
	delete(prep, "systemCopy")
	delete(prep, "summaryHistory")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["plan"] = object{"text": secret}
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
	if str(restoredPrep, "text") != secret {
		t.Fatalf("legacy copy did not survive reopen: %s", encode(restoredPrep))
	}

	// Withdraw the material after the reopen and run the real local tick.
	restored["facts"] = []any{}
	if made, changed, err := RunProactive(restored, now); err != nil || !changed || len(made) != 1 {
		t.Fatalf("tick made=%d changed=%v err=%v", len(made), changed, err)
	}
	restoredArtifact := objectsOrEmpty(restored["artifacts"])[0]
	restoredPlan := objectOrEmpty(restoredArtifact["plan"])
	if str(restoredPlan, "text") != "" || str(restoredPlan, "legacyText") != secret || restoredPlan["textNeedsReview"] != true {
		t.Fatalf("plan quarantine after reopen: %s", encode(restoredPlan))
	}
	restoredPrep = objectOrEmpty(objectsOrEmpty(restored["goals"])[0]["preparation"])
	if str(restoredPrep, "text") != "" || str(restoredPrep, "legacyText") != secret || restoredPrep["textNeedsReview"] != true {
		t.Fatalf("preparation quarantine after reopen: %s", encode(restoredPrep))
	}
	if strings.Contains(str(restoredArtifact, "xml"), secret) {
		t.Fatalf("tick rendered the legacy copy: %s", str(restoredArtifact, "xml"))
	}

	// Persist the quarantined state and reopen once more. The recoverable copy
	// must not be lost by the round trip.
	revision := int(number(r["revision"]))
	base := str(r, "snapshot")
	if w := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": base, "state": restored}); w["ok"] != true {
		t.Fatalf("write quarantine: %v", w)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r = bridgeRequest(t, s, object{"op": "read"})
	again := r["state"].(object)
	againPlan := objectOrEmpty(objectsOrEmpty(again["artifacts"])[0]["plan"])
	if str(againPlan, "legacyText") != secret || againPlan["textNeedsReview"] != true {
		t.Fatalf("plan quarantine lost on second reopen: %s", encode(againPlan))
	}
	againPrep := objectOrEmpty(objectsOrEmpty(again["goals"])[0]["preparation"])
	if str(againPrep, "legacyText") != secret || againPrep["textNeedsReview"] != true {
		t.Fatalf("preparation quarantine lost on second reopen: %s", encode(againPrep))
	}
}
