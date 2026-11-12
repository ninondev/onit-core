package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func grRunState() object {
	refs := []any{object{"kind": "fact", "id": "f1"}, object{"kind": "fact", "id": "f2"}}
	goal := object{
		"id": "g1", "title": "读完一本书", "step": "", "unit": "页", "target": 100.0,
		"at": "2026-11-09T00:00:00.000Z",
		"entries": []any{
			object{"id": "e1", "amount": 20.0, "note": "读完第一章", "at": "2026-11-10T10:00:00.000Z"},
			object{"id": "e2", "amount": 30.0, "note": "读完第二章", "at": "2026-11-11T10:00:00.000Z"},
		},
		"brief": object{"materialRefs": refs},
	}
	return object{
		"version": 2, "archiveId": "archive-1", "name": "", "profile": nil, "model": nil,
		"sources": []any{}, "facts": []any{
			object{"id": "f1", "text": "每天读二十页", "version": 1, "at": "2026-11-09T00:00:00.000Z"},
			object{"id": "f2", "text": "周末整理笔记", "version": 1, "at": "2026-11-09T00:00:00.000Z"},
			object{"id": "canary", "text": "这段材料没有被选中", "version": 1, "at": "2026-11-09T00:00:00.000Z"},
		},
		"discoveries": []any{}, "proposals": []any{}, "goals": []any{goal},
		"reports": []any{}, "artifacts": []any{}, "dismissed": []any{}, "conversation": []any{},
	}
}

func grReadState(t *testing.T, s *Store) (int, string, object) {
	t.Helper()
	read := bridgeRequest(t, s, object{"op": "read"})
	revision, ok := read["revision"].(int)
	if !ok {
		t.Fatalf("revision: %#v", read["revision"])
	}
	state, _ := read["state"].(object)
	return revision, str(read, "snapshot"), state
}

func grWriteState(t *testing.T, s *Store, state object) {
	t.Helper()
	revision, base, _ := grReadState(t, s)
	bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": base, "state": state})
}

func grOpenAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func grReviewIn(t *testing.T, state object) object {
	t.Helper()
	goals, _ := objects(state["goals"])
	if len(goals) != 1 {
		t.Fatalf("goals: %d", len(goals))
	}
	review := grReviewOf(goals[0])
	if review == nil {
		t.Fatal("review missing")
	}
	return review
}

func grStepStateList(review object) []string {
	steps, _ := objects(review["steps"])
	out := make([]string, len(steps))
	for i, step := range steps {
		out[i] = str(step, "status")
	}
	return out
}

func TestGoalReviewRunnerNormalPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	grWriteState(t, s, grRunState())

	review, err := s.GoalReview("", "g1", grActionStart, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(review, "status") != "running" || strings.Join(grStepStateList(review), ",") != "done,pending,pending" {
		t.Fatalf("after start: %s", encode(review))
	}
	partial, _ := review["partial"].(object)
	progress, _ := partial["progress"].(object)
	if number(progress["total"]) != 50 {
		t.Fatalf("progress total: %#v", progress["total"])
	}

	review, err = s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(grStepStateList(review), ",") != "done,done,pending" {
		t.Fatalf("after check: %s", encode(review))
	}
	materials, _ := objects(review["materialSnapshot"])
	if len(materials) != 2 || str(materials[0], "preview") == "" || str(materials[1], "preview") == "" {
		t.Fatalf("materials: %s", encode(materials))
	}
	partial, _ = review["partial"].(object)
	missing, _ := grStrings(partial["missing"])
	if !strings.Contains(strings.Join(missing, "\n"), "离目标还差 50 页") {
		t.Fatalf("missing: %s", encode(missing))
	}

	review, err = s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(review, "status") != "completed" || strings.Join(grStepStateList(review), ",") != "done,done,done" {
		t.Fatalf("after save: %s", encode(review))
	}
	result, _ := review["result"].(object)
	body := str(result, "body")
	for _, want := range []string{"已记录的进展（2 条，共 50 页）", "读完第一章", "读完第二章", "每天读二十页", "周末整理笔记", "正文：每天读二十页", "离目标还差 50 页", "未调用模型"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "这段材料没有被选中") {
		t.Fatalf("unselected canary entered body:\n%s", body)
	}
	resultMaterials, _ := objects(result["materials"])
	if len(resultMaterials) != 2 || str(resultMaterials[0], "preview") != "每天读二十页" {
		t.Fatalf("result materials: %s", encode(resultMaterials))
	}
	refs, _ := objects(result["refs"])
	if len(refs) != 2 || str(refs[0], "id") != "f1" || str(refs[1], "id") != "f2" {
		t.Fatalf("refs: %s", encode(refs))
	}

	// The result must still be readable from a new store handle.
	s.Close()
	s = grOpenAt(t, path)
	defer s.Close()
	again, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(again, "resultId") != str(review, "resultId") || str(again["result"].(object), "body") != body {
		t.Fatalf("read-back: %s", encode(again))
	}
}

func TestGoalReviewRunnerCrossProcessResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	grWriteState(t, s, grRunState())
	started, err := s.GoalReview("", "g1", grActionStart, "")
	if err != nil {
		t.Fatal(err)
	}
	reviewID := str(started, "id")
	s.Close()

	s = grOpenAt(t, path)
	shown, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(shown, "id") != reviewID || strings.Join(grStepStateList(shown), ",") != "done,pending,pending" {
		t.Fatalf("resume read: %s", encode(shown))
	}
	checked, err := s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(grStepStateList(checked), ",") != "done,done,pending" {
		t.Fatalf("resume check: %s", encode(checked))
	}
	s.Close()

	s = grOpenAt(t, path)
	defer s.Close()
	completed, err := s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(completed, "status") != "completed" || str(completed, "id") != reviewID {
		t.Fatalf("resume save: %s", encode(completed))
	}
	replayed, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(replayed, "id") != reviewID || str(replayed, "resultId") != str(completed, "resultId") {
		t.Fatalf("replay created a new result: %s", encode(replayed))
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	brief, _ := goals[0]["brief"].(object)
	if history, _ := objects(brief["reviewHistory"]); len(history) != 0 {
		t.Fatalf("replay archived a review: %s", encode(history))
	}
}

func TestGoalReviewRunnerRecoversLostSaveResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionStart, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalReview("", "g1", grActionAdvance, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	goal := goals[0]
	review := grReviewOf(goal)
	result := grBuildResult(goal, review)
	review["result"] = result
	review["resultId"] = str(result, "id")
	review["status"] = "saving"
	if step := grStepByID(review, "save"); step != nil {
		step["status"] = "running"
		step["startedAt"] = grNow()
	}
	grWriteState(t, s, state)
	s.Close()

	s = grOpenAt(t, path)
	defer s.Close()
	completed, err := s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(completed, "status") != "completed" || str(completed, "resultId") != str(result, "id") {
		t.Fatalf("lost save recovery: %s", encode(completed))
	}
	if strings.Join(grStepStateList(completed), ",") != "done,done,done" {
		t.Fatalf("steps: %s", encode(completed["steps"]))
	}
}

func TestGoalReviewRunnerTwoWorkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	first := grOpenAt(t, path)
	grWriteState(t, first, grRunState())
	second := grOpenAt(t, path)
	defer first.Close()
	defer second.Close()

	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i, store := range []*Store{first, second} {
		wg.Add(1)
		go func(i int, store *Store) {
			defer wg.Done()
			review, err := store.GoalReview("", "g1", grActionRun, "")
			if err == nil {
				ids[i] = str(review, "resultId")
			}
			errs[i] = err
		}(i, store)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	// A conflict is allowed; one bounded retry settles it.
	for i, store := range []*Store{first, second} {
		if errs[i] != nil {
			review, err := store.GoalReview("", "g1", grActionRun, "")
			if err != nil {
				t.Fatalf("retry %d: %v", i, err)
			}
			ids[i] = str(review, "resultId")
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("two workers produced different results: %q %q", ids[0], ids[1])
	}
	_, _, state := grReadState(t, first)
	review := grReviewIn(t, state)
	if str(review, "resultId") != ids[0] || str(review, "status") != "completed" {
		t.Fatalf("stored review: %s", encode(review))
	}
}

func TestGoalReviewRunnerMaterialCorrectionStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	completed, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil || str(completed, "status") != "completed" {
		t.Fatalf("run: %v %s", err, encode(completed))
	}
	var user string
	if err := s.db.QueryRow("SELECT user_id FROM android_dossier WHERE slot=1").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Correct(user, "f1", "改成每天读三十页", 1); err != nil {
		t.Fatal(err)
	}
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil || str(after, "resultId") != "" {
		t.Fatalf("correction did not stale: %s", encode(after))
	}
}

func TestGoalReviewRunnerProgressChangeStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionRun, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	entries, _ := objects(goals[0]["entries"])
	entries[0]["amount"] = 25.0
	goals[0]["entries"] = grAny(entries)
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("progress change did not stale: %s", encode(after))
	}
}

func TestGoalReviewRunnerGoalClosedStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionRun, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	goals[0]["step"] = "已达成收尾"
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("closed goal did not stale: %s", encode(after))
	}
	if _, err := s.GoalReview("", "g1", grActionStart, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed goal accepted a new review: %v", err)
	}
}

func TestGoalReviewRunnerCancelDoesNotResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionStart, ""); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.GoalReview("", "g1", grActionCancel, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(cancelled, "status") != "cancelled" {
		t.Fatalf("cancel: %s", encode(cancelled))
	}
	after, err := s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "cancelled" || after["result"] != nil {
		t.Fatalf("cancelled review resumed: %s", encode(after))
	}
	again, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(again, "status") != "cancelled" || again["result"] != nil {
		t.Fatalf("run resumed cancelled review: %s", encode(again))
	}
}

func TestGoalReviewRunnerOwnerMismatchStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionRun, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	state["archiveId"] = "another-archive"
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("owner mismatch did not stale: %s", encode(after))
	}
	if _, err := s.GoalReview("wrong-user", "g1", grActionShow, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong owner read: %v", err)
	}
}

func TestGoalReviewRunnerRejectsForgedSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionStart, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	review := grReviewOf(goals[0])
	review["selected"] = []any{object{"kind": "fact", "id": "f1"}, object{"kind": "fact", "id": "canary"}}
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged selection accepted: %v", err)
	}
}

func TestGoalReviewRunnerEmptyInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	if _, err := s.GoalReview("", "", grActionShow, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty goal: %v", err)
	}
	if _, err := s.GoalReview("", "g1", grActionStart, strings.Repeat("x", grMaxRequest+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized request: %v", err)
	}
	if _, err := s.GoalReview("", "g1", "unknown", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown action: %v", err)
	}
}

// 材料原句原样进入快照；「尚未取得」这类语义判断不做，交给获授权的模型。
func TestGoalReviewRunnerQuotesMaterialVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	state := grRunState()
	facts, _ := objects(state["facts"])
	facts[0]["text"] = "每天读二十页。校对签字尚未取得。印刷样张尚未取得。"
	state["facts"] = grAny(facts)
	grWriteState(t, s, state)
	review, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := review["result"].(object)
	body := str(result, "body")
	for _, want := range []string{"校对签字尚未取得", "印刷样张尚未取得", "未调用模型"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "规则核对") || strings.Contains(body, "原文提到") {
		t.Fatalf("lexical gap analysis residue in body:\n%s", body)
	}
	missing, _ := grStrings(result["missing"])
	if len(missing) < 2 {
		t.Fatalf("missing: %s", encode(missing))
	}
}

func TestGoalReviewRunnerFirstStepFreezesMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionStart, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	facts, _ := objects(state["facts"])
	facts[0]["text"] = "每天读三十页"
	facts[0]["version"] = 2.0
	state["facts"] = grAny(facts)
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionAdvance, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("material changed after the first step but the task continued: %s", encode(after))
	}
}

func TestGoalReviewRunnerWithdrawalSavesAndStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionRun, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	brief, _ := goals[0]["brief"].(object)
	brief["materialRefs"] = []any{object{"kind": "fact", "id": "f1"}}
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("withdrawal did not stale the result: %s", encode(after))
	}
	if !strings.Contains(str(after, "staleReason"), "范围") {
		t.Fatalf("stale reason: %s", encode(after))
	}
}

func grStateWithArtifact() object {
	state := grRunState()
	state["artifacts"] = []any{object{
		"id": "a1", "goalId": "g1", "type": "prepared", "xml": "<card>准备</card>",
		"proactive": object{"kind": "plan", "title": "准备卡"},
	}}
	return state
}

func TestGoalReviewRunnerDeletedArtifactStales(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grStateWithArtifact())
	completed, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := completed["result"].(object)
	artifacts, _ := objects(result["artifacts"])
	if len(artifacts) != 1 {
		t.Fatalf("result artifacts: %s", encode(artifacts))
	}
	_, _, state := grReadState(t, s)
	state["artifacts"] = []any{}
	grWriteState(t, s, state)
	after, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(after, "status") != "stale" || after["result"] != nil {
		t.Fatalf("deleted artifact did not stale the result: %s", encode(after))
	}
}

func TestGoalReviewRunnerTitleOnlyArtifactNotReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	state := grRunState()
	state["artifacts"] = []any{object{"id": "a1", "goalId": "g1", "type": "prepared", "title": "只有标题"}}
	grWriteState(t, s, state)
	review, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := review["result"].(object)
	artifacts, _ := objects(result["artifacts"])
	if len(artifacts) != 0 {
		t.Fatalf("title-only artifact counted as readable: %s", encode(artifacts))
	}
	if !strings.Contains(str(result, "body"), "没有可打开的准备产物") {
		t.Fatalf("body: %s", str(result, "body"))
	}
}

func TestGoalReviewRunnerRejectsForgedArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grStateWithArtifact())
	if _, err := s.GoalReview("", "g1", grActionRun, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	review := grReviewOf(goals[0])
	result, _ := review["result"].(object)
	result["artifacts"] = []any{object{"id": "ghost", "kind": "prepared", "title": "伪造"}}
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged artifact accepted: %v", err)
	}
}

func TestGoalReviewRunnerRecoversRunningStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	grWriteState(t, s, grRunState())
	if _, err := s.GoalReview("", "g1", grActionStart, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalReview("", "g1", grActionAdvance, ""); err != nil {
		t.Fatal(err)
	}
	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	review := grReviewOf(goals[0])
	if step := grStepByID(review, "check"); step != nil {
		step["status"] = "running"
		step["startedAt"] = grNow()
	}
	grWriteState(t, s, state)
	s.Close()

	s = grOpenAt(t, path)
	defer s.Close()
	completed, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(completed, "status") != "completed" || strings.Join(grStepStateList(completed), ",") != "done,done,done" {
		t.Fatalf("running step was not recovered durably: %s", encode(completed))
	}
}

func grStateWithWeeklyReport(count float64) object {
	state := grRunState()
	state["reports"] = []any{object{
		"id": "weekly-1", "at": "2026-11-11T12:00:00.000Z", "from": "2026-11-06T00:00:00.000Z",
		"rows": []any{object{
			"goalId": "g1", "title": "读完一本书", "step": "", "unit": "页",
			"count": count, "amount": 50.0, "notes": []any{"读完第一章", "读完第二章"},
		}},
	}}
	return state
}

// A legitimate weekly report is a readable artifact. splitDossier moves reports
// into rows, so validation must read them from the split dossier; otherwise a
// completed review that cites the report stales itself on the next write.
func TestGoalReviewRunnerWeeklyReportArtifactStaysReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grStateWithWeeklyReport(2))

	review, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(review, "status") != "completed" {
		t.Fatalf("run: %s", encode(review))
	}
	result, _ := review["result"].(object)
	artifacts, _ := objects(result["artifacts"])
	found := false
	for _, artifact := range artifacts {
		if str(artifact, "id") == "weekly-1" && str(artifact, "kind") == "report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("weekly report omitted: %s", encode(artifacts))
	}

	_, _, state := grReadState(t, s)
	if _, err := splitDossier(state); err != nil {
		t.Fatalf("weekly report artifact staled on validation: %v", err)
	}
	revision, base, _ := grReadState(t, s)
	reply := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": base, "state": state})
	if reply["ok"] != true {
		t.Fatalf("write with weekly report refused: %s", encode(reply))
	}
	shown, err := s.GoalReview("", "g1", grActionShow, "")
	if err != nil {
		t.Fatal(err)
	}
	if str(shown, "status") != "completed" {
		t.Fatalf("after write: %s", encode(shown))
	}
}

// An empty weekly report is not a readable artifact, and a completed result
// that cites it is forged, not merely stale.
func TestGoalReviewRunnerEmptyWeeklyReportNotReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s := grOpenAt(t, path)
	defer s.Close()
	grWriteState(t, s, grStateWithWeeklyReport(0))

	review, err := s.GoalReview("", "g1", grActionRun, "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := review["result"].(object)
	artifacts, _ := objects(result["artifacts"])
	for _, artifact := range artifacts {
		if str(artifact, "id") == "weekly-1" {
			t.Fatalf("empty weekly report counted as readable: %s", encode(artifacts))
		}
	}

	_, _, state := grReadState(t, s)
	goals, _ := objects(state["goals"])
	forged := grReviewOf(goals[0])
	forgedResult, _ := forged["result"].(object)
	forgedResult["artifacts"] = []any{object{"id": "weekly-1", "kind": "report", "title": "伪造周报"}}
	if _, err := splitDossier(state); !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged empty weekly report accepted: %v", err)
	}
}
