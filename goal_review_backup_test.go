package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoalReviewBackupRestoreKeepsReadableResult(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	if err := runInit([]string{"-dir", srcDir, "-owner", "owner-a"}); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(srcDir, "credentials", "owner-a.token"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(srcDir, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.User(strings.TrimSpace(string(token)))
	if err != nil {
		t.Fatal(err)
	}
	src, fact, goal, _ := seedFollowReview(t, s, user)
	feed := enableFollowReview(t, s, user, goal, src, fact, "en-1", 0)
	work := followWork(t, feed)
	if work["status"] != "completed" {
		t.Fatalf("status=%v", work["status"])
	}
	result, _ := work["result"].(object)
	body := str(result, "body")
	if !strings.Contains(body, "校对签字尚未取得") || !strings.Contains(body, "13") {
		t.Fatalf("setup body=%s", body)
	}
	resultID := str(result, "id")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	bak := filepath.Join(root, "bak")
	if err := runBackup([]string{"-dir", srcDir, "-out", bak}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := runRestore([]string{"-backup", bak, "-dir", dst}); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(filepath.Join(dst, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	again, err := restored.GoalReviewFeed(user, goal, "")
	if err != nil {
		t.Fatal(err)
	}
	againWork := followWork(t, again)
	if againWork["status"] != "completed" {
		t.Fatalf("restored status=%v", againWork["status"])
	}
	againResult, _ := againWork["result"].(object)
	if str(againResult, "id") != resultID || str(againResult, "body") != body {
		t.Fatalf("restored result drifted id=%s body=%s", str(againResult, "id"), str(againResult, "body"))
	}
}

func TestGoalReviewBackupRestoreKeepsHandledAndWaiting(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	if err := runInit([]string{"-dir", srcDir, "-owner", "owner-a"}); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(srcDir, "credentials", "owner-a.token"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(srcDir, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.User(strings.TrimSpace(string(token)))
	if err != nil {
		t.Fatal(err)
	}
	srcH, factH, goalH, _ := seedFollowReview(t, s, user)
	srcW, factW, goalW, _ := seedFollowReview(t, s, user)
	handledFeed := enableFollowReview(t, s, user, goalH, srcH, factH, "en-handled", 0)
	waitingFeed := enableFollowReview(t, s, user, goalW, srcW, factW, "en-waiting", 0)
	handledWork := followWork(t, handledFeed)
	waitingWork := followWork(t, waitingFeed)
	handledResult, _ := handledWork["result"].(object)
	waitingResult, _ := waitingWork["result"].(object)
	handledID := str(handledResult, "id")
	waitingID := str(waitingResult, "id")
	handledBody := str(handledResult, "body")
	waitingBody := str(waitingResult, "body")
	if _, err := s.GoalReviewFeedback(user, goalH, goalReviewFeedbackInput{
		RequestID: "handled-bak", Action: grFollowHandled, ResultID: handledID,
		ResultRevision: mustInt(handledWork["resultRevision"]), Note: "合成明确反馈",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalReviewFeedback(user, goalW, goalReviewFeedbackInput{
		RequestID: "waiting-bak", Action: grFollowWaiting, ResultID: waitingID,
		ResultRevision: mustInt(waitingWork["resultRevision"]), Note: "在等印刷样张",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	bak := filepath.Join(root, "bak")
	if err := runBackup([]string{"-dir", srcDir, "-out", bak}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := runRestore([]string{"-backup", bak, "-dir", dst}); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(filepath.Join(dst, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close() })
	againH, err := restored.GoalReviewFeed(user, goalH, "")
	if err != nil {
		t.Fatal(err)
	}
	againW, err := restored.GoalReviewFeed(user, goalW, "")
	if err != nil {
		t.Fatal(err)
	}
	hw := followWork(t, againH)
	ww := followWork(t, againW)
	hr, _ := hw["result"].(object)
	wr, _ := ww["result"].(object)
	hf, _ := hw["feedback"].(object)
	wf, _ := ww["feedback"].(object)
	if str(hr, "id") != handledID || str(hr, "body") != handledBody {
		t.Fatalf("handled result drifted id=%s", str(hr, "id"))
	}
	if str(wr, "id") != waitingID || str(wr, "body") != waitingBody {
		t.Fatalf("waiting result drifted id=%s", str(wr, "id"))
	}
	if str(hf, "action") != grFollowHandled {
		t.Fatalf("handled feedback lost: %+v", hf)
	}
	if str(wf, "action") != grFollowWaiting || str(wf, "note") != "在等印刷样张" {
		t.Fatalf("waiting feedback lost: %+v", wf)
	}
}
