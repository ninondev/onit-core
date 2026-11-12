package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func grTestState() object {
	refs := []any{object{"kind": "fact", "id": "f1"}, object{"kind": "fact", "id": "f2"}}
	materials := []any{
		object{"kind": "fact", "id": "f1", "label": "每天读二十页", "version": 1, "textHash": grHash("每天读二十页"), "at": "2026-11-09T00:00:00.000Z", "preview": "每天读二十页"},
		object{"kind": "fact", "id": "f2", "label": "周末整理笔记", "version": 1, "textHash": grHash("周末整理笔记"), "at": "2026-11-09T00:00:00.000Z", "preview": "周末整理笔记"},
	}
	progress := object{
		"total": 50.0,
		"entries": []any{
			object{"id": "e1", "amount": 20.0, "note": "读完第一章", "at": "2026-11-10T10:00:00.000Z"},
			object{"id": "e2", "amount": 30.0, "note": "读完第二章", "at": "2026-11-11T10:00:00.000Z"},
		},
	}
	goal := object{
		"id": "g1", "title": "读完一本书", "step": "", "unit": "页", "at": "2026-11-09T00:00:00.000Z",
		"entries": []any{
			object{"id": "e1", "amount": 20.0, "note": "读完第一章", "at": "2026-11-10T10:00:00.000Z"},
			object{"id": "e2", "amount": 30.0, "note": "读完第二章", "at": "2026-11-11T10:00:00.000Z"},
		},
	}
	review := object{
		"id": "gr_1", "owner": "archive-1", "goalId": "g1",
		"request": "整理这段已记录进展，结合我选的材料，列出做过什么、有何产物、仍缺什么依据、下一步待确认什么",
		"status":  "completed", "selected": refs, "progressEntryIds": []any{"e1", "e2"},
		"progressSnapshot": progress, "materialSnapshot": materials,
		"steps": []any{
			object{"id": "progress", "status": "done"},
			object{"id": "check", "status": "done"},
			object{"id": "save", "status": "done"},
		},
		"attempts": 1, "resultId": "gr_result_1",
		"result": object{
			"id": "gr_result_1", "reviewId": "gr_1", "goalId": "g1",
			"createdAt": "2026-11-11T12:00:00.000Z", "request": "整理这段已记录进展",
			"progress": progress, "artifacts": []any{}, "materials": materials,
			"missing": []any{"离目标还差 50 页"}, "refs": refs,
			"body": "目标复盘：读完一本书\n已记录的进展（2 条，共 50 页）\n这份复盘由本机规则整理，没有调用模型。",
		},
	}
	goal["brief"] = object{"materialRefs": refs, "review": review}
	review["selectionHash"] = grSelectionHash(goal)
	return object{
		"version": 2, "archiveId": "archive-1", "name": "", "profile": nil, "model": nil,
		"sources": []any{}, "facts": []any{
			object{"id": "f1", "text": "每天读二十页", "version": 1, "at": "2026-11-09T00:00:00.000Z"},
			object{"id": "f2", "text": "周末整理笔记", "version": 1, "at": "2026-11-09T00:00:00.000Z"},
		},
		"discoveries": []any{}, "proposals": []any{}, "goals": []any{goal},
		"reports": []any{}, "dismissed": []any{}, "conversation": []any{},
	}
}

func TestGoalReviewSelectionHashMatchesJS(t *testing.T) {
	state := grTestState()
	goals, _ := objects(state["goals"])
	if got := grSelectionHash(goals[0]); got != "e767893a" {
		t.Fatalf("selection hash %s", got)
	}
}

func TestGoalReviewDossierRoundTripAndServiceInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := grTestState()
	if r := bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state}); r["revision"] != 1 {
		t.Fatal(r)
	}
	read := bridgeRequest(t, s, object{"op": "read"})
	got := read["state"].(object)
	goals, _ := objects(got["goals"])
	brief := goals[0]["brief"].(object)
	review := brief["review"].(object)
	if review["status"] != "completed" || review["resultId"] != "gr_result_1" {
		t.Fatalf("round trip review: %s", encode(review))
	}
	var user string
	if err := s.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Correct(user, "f1", "改成每天读三十页", 1); err != nil {
		t.Fatal(err)
	}
	after := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	goals, _ = objects(after["goals"])
	review = goals[0]["brief"].(object)["review"].(object)
	if review["status"] != "stale" || review["result"] != nil || review["resultId"] != "" {
		t.Fatalf("service-side correction did not invalidate: %s", encode(review))
	}
}

func TestGoalReviewRejectsForgedCompletedResult(t *testing.T) {
	state := grTestState()
	goals, _ := objects(state["goals"])
	review := goals[0]["brief"].(object)["review"].(object)
	result := review["result"].(object)
	result["progress"] = object{
		"total": 129.0,
		"entries": []any{
			object{"id": "e1", "amount": 99.0, "note": "读完第一章", "at": "2026-11-10T10:00:00.000Z"},
			object{"id": "e2", "amount": 30.0, "note": "读完第二章", "at": "2026-11-11T10:00:00.000Z"},
		},
	}
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged progress accepted: %v", err)
	}

	state = grTestState()
	goals, _ = objects(state["goals"])
	review = goals[0]["brief"].(object)["review"].(object)
	review["resultId"] = "gr_other"
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged result id accepted: %v", err)
	}

	state = grTestState()
	goals, _ = objects(state["goals"])
	review = goals[0]["brief"].(object)["review"].(object)
	review["selected"] = []any{object{"kind": "fact", "id": "f1"}}
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged selection accepted: %v", err)
	}
}

func TestGoalReviewOwnerMismatchBecomesStale(t *testing.T) {
	state := grTestState()
	goals, _ := objects(state["goals"])
	review := goals[0]["brief"].(object)["review"].(object)
	review["owner"] = "another-archive"
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	review = d.rows["goals"][0]["brief"].(object)["review"].(object)
	if review["status"] != "stale" || review["result"] != nil {
		t.Fatalf("owner mismatch not staled: %s", encode(review))
	}
}

func TestGoalReviewMaterialVersionChangeBecomesStale(t *testing.T) {
	state := grTestState()
	facts, _ := objects(state["facts"])
	facts[0]["text"] = "每天读三十页"
	facts[0]["version"] = 2
	state["facts"] = grAny(facts)
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	review := d.rows["goals"][0]["brief"].(object)["review"].(object)
	if review["status"] != "stale" || review["result"] != nil {
		t.Fatalf("material version change not staled: %s", encode(review))
	}
}
