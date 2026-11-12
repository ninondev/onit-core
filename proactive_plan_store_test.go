package main

import (
	"strings"
	"testing"
	"time"
)

// The scheduled preparation must survive the real dossier path, not only an
// in-memory object: write the authorized inputs, tick, edit the result, write
// with the returned revision, tick again and read the persisted card back.
func TestSchedulePlanPersistsThroughRealStore(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	tick, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	tickState := tick["state"].(object)
	plan := findPreparedKind(tickState, "schedule-plan")
	if plan == nil {
		t.Fatalf("tick did not persist a schedule-plan card: %s", encode(tick))
	}
	xml := str(plan, "xml")
	for _, want := range []string{`kind="schedule-plan"`, `<test>合成测试</test>`, "合成材料", "先看材料"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("persisted plan missing %q: %s", want, xml)
		}
	}

	goals := objectsOrEmpty(tickState["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["text"] = "真实存储里的用户版本"
	goals[0]["preparation"] = prep
	tickState["goals"] = toAnyObjects(goals)
	revision := int(number(tick["revision"]))
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": revision, "base": str(tick, "snapshot"), "state": tickState}))); err != nil {
		t.Fatalf("write edited preparation: %v", err)
	}
	tick2, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	plan2 := findPreparedKind(tick2["state"].(object), "schedule-plan")
	if plan2 == nil {
		t.Fatal("second tick lost the plan card")
	}
	xml2 := str(plan2, "xml")
	if !strings.Contains(xml2, "真实存储里的用户版本") || !strings.Contains(xml2, `<result kind="user">`) {
		t.Fatalf("persisted plan did not use the user text: %s", xml2)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	readGoals := objectsOrEmpty(read["state"].(object)["goals"])
	readPrep := objectOrEmpty(readGoals[0]["preparation"])
	if str(readPrep, "text") != "真实存储里的用户版本" {
		t.Fatalf("user text was not persisted: %s", encode(readPrep))
	}
}

func findPreparedKind(state object, kind string) object {
	for _, artifact := range objectsOrEmpty(state["artifacts"]) {
		if str(artifact, "type") != "prepared" {
			continue
		}
		if str(objectOrEmpty(artifact["proactive"]), "kind") == kind {
			return artifact
		}
	}
	return nil
}
