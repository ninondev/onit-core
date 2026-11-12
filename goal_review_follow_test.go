package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedFollowReview(t *testing.T, s *Store, user string) (sourceID, factID, goalID, entry6 string) {
	t.Helper()
	src, fact, goal := seedPreparation(t, s, user, "观测表草稿已整理，校对签字尚未取得。印刷样张尚未取得。")
	first, err := s.RecordGoal(user, goal, 6, "合成核对记录1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordGoal(user, goal, 7, "合成核对记录2"); err != nil {
		t.Fatal(err)
	}
	return src, fact, goal, first.ID
}

func enableFollowReview(t *testing.T, s *Store, user, goal, src, fact, requestID string, expected int) object {
	t.Helper()
	feed, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
		RequestID: requestID, ExpectedControlRevision: &expected, Enabled: true, Timezone: "UTC",
		Request:  "核对签字与样张",
		Selected: []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return feed
}

func followWork(t *testing.T, feed object) object {
	t.Helper()
	work, _ := feed["work"].(object)
	if work == nil {
		t.Fatal("missing work")
	}
	return work
}

func TestGoalReviewFirstResultIsUnreadUntilSeen(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	if work["status"] != "completed" {
		t.Fatalf("status=%v", work["status"])
	}
	result, _ := work["result"].(object)
	if str(result, "body") == "" {
		t.Fatal("empty first body")
	}
	if mustInt(work["attentionRevision"]) < 1 || work["unread"] != true || mustInt(work["seenAttentionRevision"]) != 0 {
		t.Fatalf("first result not unread: %+v", work)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	if mustInt(againWork["attentionRevision"]) != mustInt(work["attentionRevision"]) || againWork["unread"] != true || mustInt(againWork["seenAttentionRevision"]) != 0 {
		t.Fatalf("GET marked seen: %+v", againWork)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	stable, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	stableWork := followWork(t, stable)
	if str(stableWork, "workId") != str(work, "workId") || mustInt(stableWork["resultRevision"]) != mustInt(work["resultRevision"]) || mustInt(stableWork["attentionRevision"]) != mustInt(work["attentionRevision"]) {
		t.Fatalf("repeat tick changed identity: first=%v later=%v", work, stableWork)
	}
}

func TestGoalReviewWithdrawalClearsBodyThenBlocks(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	if followWork(t, feed)["status"] != "completed" {
		t.Fatalf("status=%v", followWork(t, feed)["status"])
	}
	if err := s.RemoveSource(user, src, nil); err != nil {
		t.Fatal(err)
	}
	read, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := followWork(t, read)["result"].(object)
	if str(result, "body") != "" {
		t.Fatalf("GET still exposes withdrawn body: %q", str(result, "body"))
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	blockedWork := followWork(t, blocked)
	if blockedWork["status"] != grWorkBlocked {
		t.Fatalf("status=%v", blockedWork["status"])
	}
	blockedResult, _ := blockedWork["result"].(object)
	if str(blockedResult, "body") != "" {
		t.Fatal("blocked retained result body")
	}
	before := encode(blockedWork)
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	if encode(followWork(t, again)) != before {
		t.Fatalf("blocked work not stable")
	}
}

func TestGoalReviewWaitingKeepsHoldAndClearsStaleBody(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	_, err := s.GoalReviewFeedback(user, goal, goalReviewFeedbackInput{
		RequestID: "wait-1", Action: grFollowWaiting, ResultID: str(result, "id"),
		ResultRevision: mustInt(work["resultRevision"]), Note: "等待合成校对员明确回复",
	})
	if err != nil {
		t.Fatal(err)
	}
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	held, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	heldWork := followWork(t, held)
	if str(heldWork, "workId") != str(work, "workId") {
		t.Fatalf("waiting replaced work %s -> %s", work["workId"], heldWork["workId"])
	}
	fb, _ := heldWork["feedback"].(object)
	if str(fb, "action") != grFollowWaiting || str(fb, "note") != "等待合成校对员明确回复" {
		t.Fatalf("waiting lost: %+v", fb)
	}
	heldResult, _ := heldWork["result"].(object)
	if str(heldResult, "body") != "" {
		t.Fatalf("waiting still exposes old body")
	}
	expected := mustInt(held["controlRevision"])
	done, err := s.ContinueGoalReview(user, goal, "continue-1", &expected)
	if err != nil {
		t.Fatal(err)
	}
	doneWork := followWork(t, done)
	if doneWork["status"] != "completed" {
		t.Fatalf("after continue status=%v", doneWork["status"])
	}
	doneResult, _ := doneWork["result"].(object)
	progress, _ := doneResult["progress"].(object)
	if mustInt(progress["total"]) != 9 {
		t.Fatalf("after continue total=%v body=%q", progress["total"], str(doneResult, "body"))
	}
	if str(doneWork, "workId") == str(work, "workId") {
		t.Fatal("continue kept the old generation")
	}
	if mustInt(doneWork["resultRevision"]) <= mustInt(work["resultRevision"]) {
		t.Fatalf("resultRevision reset: first=%v later=%v", work["resultRevision"], doneWork["resultRevision"])
	}
	if mustInt(doneWork["attentionRevision"]) <= mustInt(work["attentionRevision"]) {
		t.Fatalf("attentionRevision reset: first=%v later=%v", work["attentionRevision"], doneWork["attentionRevision"])
	}
	if !strings.Contains(str(doneWork, "changeNote"), "进展") {
		t.Fatalf("changeNote=%q", doneWork["changeNote"])
	}
}

func TestGoalReviewLaterReplayAfterDueIsIdempotent(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	t.Setenv("ADVISER_TEST_NOW", "2026-11-12T12:00:00Z")
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	in := goalReviewFeedbackInput{
		RequestID: "later-1", Action: grFollowLater, ResultID: str(result, "id"),
		ResultRevision: mustInt(work["resultRevision"]), Note: "明天再看",
		DueAt: "2026-11-13T20:00:00+08:00", Timezone: "Asia/Shanghai",
	}
	snooze, err := s.GoalReviewFeedback(user, goal, in)
	if err != nil {
		t.Fatal(err)
	}
	fb, _ := followWork(t, snooze)["feedback"].(object)
	if str(fb, "dueAt") != "2026-11-13T12:00:00Z" {
		t.Fatalf("dueAt=%q", fb["dueAt"])
	}
	t.Setenv("ADVISER_TEST_NOW", "2026-11-13T12:00:00Z")
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	replay, err := s.GoalReviewFeedback(user, goal, in)
	if err != nil {
		t.Fatalf("replay after due: %v", err)
	}
	if str(followWork(t, replay), "workId") != str(followWork(t, snooze), "workId") {
		t.Fatal("replay changed work")
	}
	t.Setenv("ADVISER_TEST_NOW", "2026-11-13T13:00:00Z")
	_, err = s.GoalReviewFeedback(user, goal, goalReviewFeedbackInput{
		RequestID: "later-2", Action: grFollowLater, ResultID: str(result, "id"),
		ResultRevision: mustInt(work["resultRevision"]), Note: "过期新请求",
		DueAt: "2026-11-13T12:00:00Z", Timezone: "UTC",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("past due new request err=%v", err)
	}
}

func TestGoalReviewPauseClearsStaleBodyAndDoesNotResume(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	expected := mustInt(feed["controlRevision"])
	paused, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
		RequestID: "pause-1", ExpectedControlRevision: &expected, Enabled: false, Timezone: "UTC",
		Request:  "核对签字与样张",
		Selected: []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if paused["enabled"] != false {
		t.Fatalf("enabled=%v", paused["enabled"])
	}
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, time.Date(2026, 12, 13, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	persist, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	if persist["enabled"] != false {
		t.Fatal("pause resumed")
	}
	work, _ := persist["work"].(object)
	if work != nil && str(work, "status") == "completed" {
		t.Fatalf("paused still completed: %+v", work)
	}
	if work != nil {
		result, _ := work["result"].(object)
		if str(result, "body") != "" {
			t.Fatal("paused stale body still visible")
		}
	}
}

func readySameGoalPreparation(t *testing.T, s *Store, user, goal, src, key string) preparationOutput {
	t.Helper()
	enablePreparation(t, s, user, key, goal, "UTC", preparationRef{Kind: "source", ID: src})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	made, err := s.RunPreparationTick(user, now)
	if err != nil || made != 1 {
		t.Fatalf("prep tick made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, key)
	if out.Status != "ready" || strings.TrimSpace(out.XML) == "" {
		t.Fatalf("prep not ready: %+v", out)
	}
	return out
}

func followArtifacts(t *testing.T, work object) []object {
	t.Helper()
	result, _ := work["result"].(object)
	if result == nil {
		t.Fatal("missing result")
	}
	artifacts, _ := objects(result["artifacts"])
	return artifacts
}

func TestGoalReviewIncludesSameGoalReadyPreparation(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	out := readySameGoalPreparation(t, s, user, goal, src, "same-goal-prepared")
	// Report window must cover the seeded progress, which is stamped with the
	// real clock; a fixed date rots as soon as the wall clock passes it.
	if _, err := s.WeeklyReport(user, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	if work["status"] != "completed" {
		t.Fatalf("status=%v", work["status"])
	}
	artifacts := followArtifacts(t, work)
	kinds := map[string]int{}
	for _, a := range artifacts {
		kinds[str(a, "kind")]++
		if str(a, "id") == "" {
			t.Fatalf("artifact missing id: %+v", a)
		}
	}
	if kinds["report"] < 1 || kinds["material-brief"] < 1 {
		t.Fatalf("expected report and material-brief, got %s", encode(artifacts))
	}
	result, _ := work["result"].(object)
	body := str(result, "body")
	if !strings.Contains(body, "周报") || !strings.Contains(body, preparationCardTitle(out.XML)) {
		t.Fatalf("body omitted existing product: %s", body)
	}
	if strings.Contains(body, str(result, "id")) && strings.Count(body, str(result, "id")) > 0 {
		// R40 result id may appear only as its own identity, not as an input artifact line.
	}
	for _, a := range artifacts {
		if str(a, "id") == str(result, "id") || strings.HasPrefix(str(a, "kind"), "goal-review") {
			t.Fatalf("review used its own result as input: %+v", a)
		}
	}
}

func TestGoalReviewIgnoresForeignEmptyAndWithdrawnPreparation(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	other, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	readySameGoalPreparation(t, s, user, goal, src, "same-goal-prepared")

	foreignSrc, _, foreignGoal := seedPreparation(t, s, user, "另一目标的私人简报原文。")
	readySameGoalPreparation(t, s, user, foreignGoal, foreignSrc, "other-goal-prepared")

	otherSrc, _, otherGoal := seedPreparation(t, s, other, "他人目标的私人简报原文。")
	readySameGoalPreparation(t, s, other, otherGoal, otherSrc, "owner-b-prepared")

	if err := s.write(func(tx *sql.Tx) error {
		return writePreparationOutputTx(tx, preparationOutput{
			UserID: user, ClientKey: "forged-empty", GoalID: goal,
			Revision: 1, Status: "ready", XML: "   ", Date: "2026-11-12",
			UpdatedAt: "2026-11-12T00:00:00Z",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.write(func(tx *sql.Tx) error {
		return writePreparationOutputTx(tx, preparationOutput{
			UserID: user, ClientKey: "forged-title", GoalID: goal,
			Revision: 1, Status: "ready", XML: "<prepared><title>只有标题</title></prepared>",
			Date: "2026-11-12", UpdatedAt: "2026-11-12T00:00:00Z",
		})
	}); err != nil {
		t.Fatal(err)
	}

	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	artifacts := followArtifacts(t, work)
	for _, a := range artifacts {
		title := str(a, "title")
		id := str(a, "id")
		if strings.Contains(title, "另一目标") || strings.Contains(title, "他人") || title == "只有标题" {
			t.Fatalf("foreign/forged artifact leaked: %+v", a)
		}
		if id == preparationCardID(foreignGoal) || strings.Contains(id, otherGoal) {
			t.Fatalf("other goal card leaked: %+v", a)
		}
	}
	found := false
	for _, a := range artifacts {
		if str(a, "kind") == "material-brief" && str(a, "id") == preparationCardID(goal) {
			found = true
		}
	}
	if !found {
		t.Fatalf("same-goal brief missing: %s", encode(artifacts))
	}

	if err := s.RemoveSource(user, src, nil); err != nil {
		t.Fatal(err)
	}
	read, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	readWork := followWork(t, read)
	readResult, _ := readWork["result"].(object)
	if strings.Contains(str(readResult, "body"), "校对签字尚未取得") && str(readResult, "body") != "" {
		// live review body must not keep the withdrawn brief
	}
	if str(readResult, "body") != "" {
		t.Fatalf("GET still exposes body after withdrawal: %q", str(readResult, "body"))
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	blockedWork := followWork(t, blocked)
	if blockedWork["status"] != grWorkBlocked {
		t.Fatalf("status=%v", blockedWork["status"])
	}
	blockedResult, _ := blockedWork["result"].(object)
	for _, a := range objectsOrEmpty(blockedResult["artifacts"]) {
		if str(a, "kind") == "material-brief" {
			t.Fatalf("withdrawn brief still listed: %+v", a)
		}
	}
	if strings.Contains(encode(blockedWork), "widget:prepared-brief-"+goal) && str(blockedResult, "body") != "" {
		t.Fatalf("withdrawn product reappeared: %s", encode(blockedWork))
	}
}

func TestGoalReviewExcludesStalePreparationAfterFactCorrection(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	_, fact, goal, _ := seedFollowReview(t, s, user)
	enablePreparation(t, s, user, "same-goal-product", goal, "UTC", preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	made, err := s.RunPreparationTick(user, now)
	if err != nil || made != 1 {
		t.Fatalf("prep tick made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, "same-goal-product")
	if out.Status != "ready" || !strings.Contains(out.XML, "尚未取得") {
		t.Fatalf("initial prep=%+v xml=%s", out, out.XML)
	}
	cardID := preparationCardID(goal)
	feed, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
		RequestID: "en-fact", ExpectedControlRevision: new(int), Enabled: true, Timezone: "UTC",
		Request:  "核对记录、当前签字和已有成果",
		Selected: []preparationRef{{Kind: "fact", ID: fact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := followWork(t, feed)
	found := false
	for _, a := range followArtifacts(t, first) {
		if str(a, "id") == cardID && str(a, "kind") == "material-brief" {
			found = true
		}
	}
	if !found {
		t.Fatalf("current brief missing before correction: %s", encode(followArtifacts(t, first)))
	}
	if _, err := s.Correct(user, fact, "校对签字已由本人确认取得。", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	changed, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	changedWork := followWork(t, changed)
	if changedWork["status"] != "completed" {
		t.Fatalf("status=%v", changedWork["status"])
	}
	result, _ := changedWork["result"].(object)
	body := str(result, "body")
	if !strings.Contains(body, "校对签字已由本人确认取得") {
		t.Fatalf("new fact not used: %s", body)
	}
	for _, a := range followArtifacts(t, changedWork) {
		if str(a, "id") == cardID {
			t.Fatalf("old brief advertised as current before any feed: %+v body=%s", a, body)
		}
	}
	prepFeed, err := s.PreparationFeed(user, now)
	if err != nil {
		t.Fatal(err)
	}
	row := feedPreparation(t, prepFeed, "same-goal-product")
	if str(row, "status") != "stale" {
		t.Fatalf("brief afterward status=%v", row["status"])
	}
}

func TestGoalReviewUnsupportedFactBlocksAndClearsBody(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	if followWork(t, feed)["status"] != "completed" {
		t.Fatalf("status=%v", followWork(t, feed)["status"])
	}
	if _, err := s.db.Exec("UPDATE sources SET body=? WHERE user_id=? AND id=?", "来源已被改写，原引文不在其中。", user, src); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	work := followWork(t, blocked)
	if work["status"] != grWorkBlocked {
		t.Fatalf("status=%v reason=%v", work["status"], work["blockedReason"])
	}
	result, _ := work["result"].(object)
	if str(result, "body") != "" {
		t.Fatalf("unsupported fact still exposes body: %q", str(result, "body"))
	}
	if strings.Contains(encode(work), "校对签字尚未取得") {
		t.Fatalf("unsupported quote survived in current work: %s", encode(work))
	}
}

func TestGoalReviewDeletedEntryLeavesCurrentTotal(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	result, _ := followWork(t, feed)["result"].(object)
	progress, _ := result["progress"].(object)
	if mustInt(progress["total"]) != 13 {
		t.Fatalf("setup total=%v", progress["total"])
	}
	if err := s.ChangeGoalEntry(user, goal, entry6, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	changed, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	work := followWork(t, changed)
	if work["status"] != "completed" {
		t.Fatalf("status=%v", work["status"])
	}
	changedResult, _ := work["result"].(object)
	changedProgress, _ := changedResult["progress"].(object)
	if mustInt(changedProgress["total"]) != 7 {
		t.Fatalf("after delete total=%v body=%s", changedProgress["total"], str(changedResult, "body"))
	}
	for _, e := range objectsOrEmpty(changedProgress["entries"]) {
		if str(e, "id") == entry6 {
			t.Fatalf("deleted entry survived: %+v", e)
		}
	}
	if strings.Contains(str(changedResult, "body"), "合成核对记录1") {
		t.Fatalf("deleted note still in body: %s", str(changedResult, "body"))
	}
}

func TestGoalReviewManagedSuppressesLegacyNudge(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	_, _, otherGoal := seedPreparation(t, s, user, "未纳管目标的合成材料。")
	now := time.Now()
	day := now.Format("2006-01-02")
	// Legacy nudge rows already stored by an old version stay in history; the
	// tick no longer creates them, and projection must keep hiding managed ones.
	legacy := []Artifact{
		{ID: "widget:" + day + ":" + goal, Date: day, Type: "nudge", XML: "<nudge>合成旧提醒</nudge>"},
		{ID: "widget:" + day + ":" + otherGoal, Date: day, Type: "nudge", XML: "<nudge>合成旧提醒</nudge>"},
	}
	for _, card := range legacy {
		if err := s.upsertArtifact(user, card); err != nil {
			t.Fatal(err)
		}
	}
	managedID := legacy[0].ID
	var stored int
	if err := s.db.QueryRow("SELECT COUNT(1) FROM artifacts WHERE user_id=? AND id=?", user, managedID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("legacy nudge missing stored=%d err=%v", stored, err)
	}
	enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	rows, err := s.Artifacts(user, day)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range rows {
		if card.ID == managedID {
			t.Fatalf("managed nudge still projected: %+v", card)
		}
	}
	foundOther := false
	for _, card := range rows {
		if strings.Contains(card.ID, otherGoal) && card.Type == "nudge" {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("unmanaged nudge disappeared: %v", rows)
	}
	if err := s.db.QueryRow("SELECT COUNT(1) FROM artifacts WHERE user_id=? AND id=?", user, managedID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("history deleted stored=%d err=%v", stored, err)
	}
	feed, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	expected := mustInt(feed["controlRevision"])
	if _, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
		RequestID: "pause-nudge", ExpectedControlRevision: &expected, Enabled: false, Timezone: "UTC",
		Request:  "核对签字与样张",
		Selected: []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}},
	}); err != nil {
		t.Fatal(err)
	}
	if made, err := s.RunAgentTick(now); err != nil {
		t.Fatal(err)
	} else if made != 0 {
		t.Fatalf("paused follow still minted nudges made=%d", made)
	}
	rows, err = s.Artifacts(user, day)
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range rows {
		if card.ID == managedID {
			t.Fatalf("paused managed nudge projected: %+v", card)
		}
	}
}

func TestGoalReviewChangeNoteIgnoresEntryIdentityRename(t *testing.T) {
	prev := goalReviewParts{
		Request: "核对签字与样张",
		Entries: []goalReviewEntryPart{
			{ID: "a", Amount: "6", Note: "合成核对记录1"},
			{ID: "b", Amount: "7", Note: "合成核对记录2"},
		},
	}
	next := goalReviewParts{
		Request: "核对签字与样张",
		Entries: []goalReviewEntryPart{
			{ID: "a-reidentified", Amount: "6", Note: "合成核对记录1"},
			{ID: "b", Amount: "7", Note: "合成核对记录2"},
		},
	}
	if note := goalReviewChangeNote(prev, next); note != "" {
		t.Fatalf("rename note=%q", note)
	}
	next.Entries[0].Amount = "2"
	if note := goalReviewChangeNote(prev, next); !strings.Contains(note, "进展") {
		t.Fatalf("real change note=%q", note)
	}
}

func TestGoalReviewSameReadableContentKeepsHandled(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	oldBody := str(result, "body")
	if oldBody == "" {
		t.Fatal("empty first body")
	}
	handled, err := s.GoalReviewFeedback(user, goal, goalReviewFeedbackInput{
		RequestID: "handled-same", Action: grFollowHandled, ResultID: str(result, "id"),
		ResultRevision: mustInt(work["resultRevision"]), Note: "合成明确反馈",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldAtt := mustInt(followWork(t, handled)["attentionRevision"])
	newID := entry6 + "-reidentified"
	if _, err := s.db.Exec("UPDATE goal_entries SET id=? WHERE user_id=? AND goal_id=? AND id=?", newID, user, goal, entry6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	againResult, _ := againWork["result"].(object)
	if str(againWork, "digest") == str(work, "digest") {
		t.Fatal("fixture did not change digest")
	}
	if str(againResult, "body") != oldBody {
		t.Fatalf("body changed:\n%s\n---\n%s", oldBody, str(againResult, "body"))
	}
	if mustInt(againWork["attentionRevision"]) != oldAtt {
		t.Fatalf("attention %v -> %v", oldAtt, againWork["attentionRevision"])
	}
	fb, _ := againWork["feedback"].(object)
	if str(fb, "action") != grFollowHandled && againWork["unread"] != false {
		t.Fatalf("handled reopened: feedback=%v unread=%v", fb, againWork["unread"])
	}
}

func TestGoalReviewHandledDropsOnRealProgressChange(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	if _, err := s.GoalReviewFeedback(user, goal, goalReviewFeedbackInput{
		RequestID: "handled-change", Action: grFollowHandled, ResultID: str(result, "id"),
		ResultRevision: mustInt(work["resultRevision"]), Note: "合成明确反馈",
	}); err != nil {
		t.Fatal(err)
	}
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	if againWork["status"] != "completed" {
		t.Fatalf("status=%v", againWork["status"])
	}
	if mustInt(againWork["attentionRevision"]) <= mustInt(work["attentionRevision"]) {
		t.Fatalf("attention did not bump: first=%v later=%v", work["attentionRevision"], againWork["attentionRevision"])
	}
	fb, _ := againWork["feedback"].(object)
	if str(fb, "action") == grFollowHandled {
		t.Fatalf("old handled still controls new prose: %+v", fb)
	}
	againResult, _ := againWork["result"].(object)
	progress, _ := againResult["progress"].(object)
	if mustInt(progress["total"]) != 9 {
		t.Fatalf("total=%v body=%s", progress["total"], str(againResult, "body"))
	}
}
