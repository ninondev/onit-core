package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGoalReviewPauseAfterSnapshotDoesNotGenerate(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setGoalReviewHook(func(stage string) error {
		if stage == "after-snapshot" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	defer setGoalReviewHook(nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
			RequestID: "en-race", ExpectedControlRevision: new(int), Enabled: true, Timezone: "UTC",
			Request:  "核对签字与样张",
			Selected: []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}},
		})
		done <- err
	}()
	select {
	case <-blocked:
	case err := <-done:
		t.Fatalf("tick finished before after-snapshot: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("after-snapshot did not fire")
	}
	expected := 1
	if _, err := s.SaveGoalReviewFollow(user, goal, goalReviewFollowInput{
		RequestID: "pause-race", ExpectedControlRevision: &expected, Enabled: false, Timezone: "UTC",
		Request:  "核对签字与样张",
		Selected: []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}},
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("enable race: %v", err)
	}
	feed, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	if feed["enabled"] != false {
		t.Fatalf("enabled=%v", feed["enabled"])
	}
	work, _ := feed["work"].(object)
	if work != nil && str(work, "status") == "completed" {
		t.Fatalf("paused first generation still completed: %+v", work)
	}
}

func TestGoalReviewHandledAfterSnapshotKeepsSameBody(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	oldBody := str(result, "body")
	resultID := str(result, "id")
	revision := mustInt(work["resultRevision"])
	oldAtt := mustInt(work["attentionRevision"])
	if _, err := s.db.Exec("UPDATE goal_entries SET id=? WHERE user_id=? AND goal_id=? AND id=?",
		entry6+"-reidentified", user, goal, entry6); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	setGoalReviewHook(func(stage string) error {
		if stage != "after-snapshot" {
			return nil
		}
		var err error
		once.Do(func() {
			_, err = s.GoalReviewFeedback(user, goal, goalReviewFeedbackInput{
				RequestID: "handled-race", Action: grFollowHandled, ResultID: resultID,
				ResultRevision: revision, Note: "合成明确反馈",
			})
		})
		return err
	})
	defer setGoalReviewHook(nil)

	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	againResult, _ := againWork["result"].(object)
	if str(againResult, "body") != oldBody {
		t.Fatalf("body changed under handled race")
	}
	if mustInt(againWork["attentionRevision"]) != oldAtt {
		t.Fatalf("attention %v -> %v", oldAtt, againWork["attentionRevision"])
	}
	fb, _ := againWork["feedback"].(object)
	if str(fb, "action") != grFollowHandled {
		t.Fatalf("handled lost: %+v", fb)
	}
}

func TestGoalReviewCorrectionAfterSnapshotUsesNewAmount(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)

	var once sync.Once
	setGoalReviewHook(func(stage string) error {
		if stage != "after-snapshot" {
			return nil
		}
		var err error
		once.Do(func() {
			amt := 2.0
			err = s.ChangeGoalEntry(user, goal, entry6, &amt, nil)
		})
		return err
	})
	defer setGoalReviewHook(nil)

	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	work := followWork(t, again)
	if work["status"] != "completed" {
		t.Fatalf("status=%v", work["status"])
	}
	result, _ := work["result"].(object)
	progress, _ := result["progress"].(object)
	if mustInt(progress["total"]) != 9 {
		t.Fatalf("old worker committed pre-correction total=%v body=%s", progress["total"], str(result, "body"))
	}
}

func TestGoalReviewRevokeAfterSnapshotDoesNotCommit(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	setGoalReviewHook(func(stage string) error {
		if stage != "after-snapshot" {
			return nil
		}
		var err error
		once.Do(func() {
			err = s.DeleteGoalReviewFollow(user, goal, "revoke-race", nil)
		})
		return err
	})
	defer setGoalReviewHook(nil)

	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalReviewFeed(user, goal, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked follow still readable: %v", err)
	}
}

func TestGoalReviewBeforeCommitFailureLeavesNoNewResult(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	oldID := str(followWork(t, feed), "workId")
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}
	setGoalReviewHook(func(stage string) error {
		if stage == "before-commit" {
			return errors.New("injected commit failure")
		}
		return nil
	})
	_, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow())
	setGoalReviewHook(nil)
	if err == nil {
		t.Fatal("expected injected commit failure")
	}
	read, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	work := followWork(t, read)
	if str(work, "workId") != oldID {
		t.Fatalf("failed commit replaced work %s -> %s", oldID, work["workId"])
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
		t.Fatalf("retry status=%v", againWork["status"])
	}
	result, _ := againWork["result"].(object)
	progress, _ := result["progress"].(object)
	if mustInt(progress["total"]) != 9 {
		t.Fatalf("retry total=%v", progress["total"])
	}
}

func TestGoalReviewSaveReadbackLossKeepsDurableResult(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal, entry6 := seedFollowReview(t, s, user)
	enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	amt := 2.0
	if err := s.ChangeGoalEntry(user, goal, entry6, &amt, nil); err != nil {
		t.Fatal(err)
	}
	setGoalReviewHook(func(stage string) error {
		if stage == "save-readback" {
			return errors.New("injected reply loss")
		}
		return nil
	})
	_, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow())
	setGoalReviewHook(nil)
	if err == nil {
		t.Fatal("expected injected reply loss")
	}
	feed, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	work := followWork(t, feed)
	result, _ := work["result"].(object)
	progress, _ := result["progress"].(object)
	if work["status"] != "completed" || mustInt(progress["total"]) != 9 {
		t.Fatalf("durable result missing after reply loss: %+v", work)
	}
	resultID := str(result, "id")
	if _, err := s.AdvanceGoalReview(user, goal, 6, goalReviewNow()); err != nil {
		t.Fatal(err)
	}
	again, err := s.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	againResult, _ := againWork["result"].(object)
	if str(againResult, "id") != resultID || mustInt(againWork["attentionRevision"]) != mustInt(work["attentionRevision"]) {
		t.Fatalf("reply loss duplicated work first=%v later=%v", work, againWork)
	}
}
