package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func coordinatorStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adviser.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func writePolicyFile(t *testing.T, dbPath, raw string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(dbPath), androidSettingsFile)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setCoordinatorClock(t *testing.T, now time.Time) {
	t.Helper()
	proactiveClock = func() time.Time { return now }
	t.Cleanup(func() { proactiveClock = time.Now })
}

func setCoordinatorHook(t *testing.T, hook func(string) error) {
	t.Helper()
	proactiveHookMu.Lock()
	proactiveTickHook = hook
	proactiveHookMu.Unlock()
	t.Cleanup(func() {
		proactiveHookMu.Lock()
		proactiveTickHook = nil
		proactiveHookMu.Unlock()
	})
}

func seedCoordinatorState(t *testing.T, s *Store, state object) {
	t.Helper()
	r := bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state})
	if r["ok"] != true {
		t.Fatalf("seed: %v", r)
	}
}

func readCoordinatorState(t *testing.T, s *Store) (object, int, string) {
	t.Helper()
	r := bridgeRequest(t, s, object{"op": "read"})
	state, _ := r["state"].(object)
	return state, int(number(r["revision"])), str(r, "snapshot")
}

func preparedCards(state object) []object {
	var out []object
	for _, artifact := range objectsOrEmpty(state["artifacts"]) {
		if str(artifact, "type") == "prepared" {
			out = append(out, artifact)
		}
	}
	return out
}

func TestProactivePolicyDefaultsAndCorruption(t *testing.T) {
	_, dbPath := coordinatorStore(t)

	missing := readProactivePolicy(dbPath)
	if missing.State != "missing" || !missing.Proactive || missing.Background {
		t.Fatalf("missing policy: %+v", missing)
	}
	if !missing.allows("foreground") || missing.allows("background") || missing.allows("maintenance") {
		t.Fatalf("missing policy permissions: %+v", missing)
	}

	writePolicyFile(t, dbPath, `{"proactive":false,"backgroundPrepare":true}`)
	off := readProactivePolicy(dbPath)
	if off.State != "ok" || off.Proactive || !off.Background {
		t.Fatalf("explicit pause: %+v", off)
	}
	if off.allows("foreground") || off.allows("background") {
		t.Fatalf("paused policy allowed generation: %+v", off)
	}

	writePolicyFile(t, dbPath, `{"proactive":true,"backgroundPrepare":true}`)
	on := readProactivePolicy(dbPath)
	if !on.allows("foreground") || !on.allows("background") {
		t.Fatalf("explicit opt-in: %+v", on)
	}

	writePolicyFile(t, dbPath, `{`)
	broken := readProactivePolicy(dbPath)
	if broken.State != "corrupt" || broken.allows("foreground") || broken.allows("background") {
		t.Fatalf("corrupt policy must deny generation: %+v", broken)
	}

	writePolicyFile(t, dbPath, `{"proactive":"false"}`)
	wrongType := readProactivePolicy(dbPath)
	if wrongType.State != "corrupt" || wrongType.allows("foreground") {
		t.Fatalf("non-boolean switch must be corruption: %+v", wrongType)
	}
}

func TestProactiveForegroundTickIsIdempotent(t *testing.T) {
	s, _ := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))

	first, err := s.runProactiveTick("foreground")
	if err != nil || first["made"] != 1 || first["changed"] != true {
		t.Fatalf("first tick: %v err=%v", first, err)
	}
	second, err := s.runProactiveTick("foreground")
	if err != nil || second["made"] != 0 || second["changed"] != false {
		t.Fatalf("second tick: %v err=%v", second, err)
	}
	state, _, _ := readCoordinatorState(t, s)
	if cards := preparedCards(state); len(cards) != 1 {
		t.Fatalf("prepared cards=%d", len(cards))
	}
}

func TestProactiveBackgroundRequiresExplicitOptIn(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	writePolicyFile(t, dbPath, `{"proactive":true}`)

	r, err := s.runProactiveTick("background")
	if err != nil || r["made"] != 0 {
		t.Fatalf("background without opt-in: %v err=%v", r, err)
	}
	state, _, _ := readCoordinatorState(t, s)
	if len(preparedCards(state)) != 0 {
		t.Fatal("background generated without explicit opt-in")
	}

	writePolicyFile(t, dbPath, `{"proactive":true,"backgroundPrepare":true}`)
	r, err = s.runProactiveTick("background")
	if err != nil || r["made"] != 1 {
		t.Fatalf("background after opt-in: %v err=%v", r, err)
	}
}

func TestProactivePauseStillMaintainsWithdrawal(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	state := schedulePlanState(now)
	seedCoordinatorState(t, s, state)
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}
	writePolicyFile(t, dbPath, `{"proactive":false}`)

	current, revision, snapshot := readCoordinatorState(t, s)
	current["facts"] = []any{}
	current["sources"] = []any{}
	if r := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": snapshot, "state": current}); r["ok"] != true {
		t.Fatalf("withdraw: %v", r)
	}

	r, err := s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != true {
		t.Fatalf("paused maintenance: %v err=%v", r, err)
	}
	after, _, _ := readCoordinatorState(t, s)
	cards := preparedCards(after)
	if len(cards) != 1 || str(objectOrEmpty(cards[0]["proactive"]), "status") != "stale" {
		t.Fatalf("card not stale: %v", cards)
	}
	if strings.Contains(str(cards[0], "xml"), "合成材料") {
		t.Fatalf("withdrawn text remained: %s", str(cards[0], "xml"))
	}
	if basis := objectsOrEmpty(objectOrEmpty(cards[0]["proactive"])["basis"]); len(basis) != 0 {
		t.Fatalf("withdrawn basis remained: %v", basis)
	}
	again, err := s.runProactiveTick("foreground")
	if err != nil || again["made"] != 0 || again["changed"] != false {
		t.Fatalf("repeat maintenance: %v err=%v", again, err)
	}

	// Resuming must not bring the withdrawn original back.
	writePolicyFile(t, dbPath, `{"proactive":true}`)
	if r, err := s.runProactiveTick("foreground"); err != nil {
		t.Fatalf("resume: %v err=%v", r, err)
	}
	resumed, _, _ := readCoordinatorState(t, s)
	for _, card := range preparedCards(resumed) {
		if strings.Contains(str(card, "xml"), "合成材料") {
			t.Fatalf("withdrawn text revived: %s", str(card, "xml"))
		}
	}
}

func TestProactivePauseStillMaintainsCorrection(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}
	writePolicyFile(t, dbPath, `{"proactive":false}`)

	current, revision, snapshot := readCoordinatorState(t, s)
	facts := objectsOrEmpty(current["facts"])
	facts[0]["text"] = "更正后的材料"
	facts[0]["quote"] = "更正后的材料"
	facts[0]["version"] = 2
	current["facts"] = toAnyObjects(facts)
	if r := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": snapshot, "state": current}); r["ok"] != true {
		t.Fatalf("correct: %v", r)
	}

	r, err := s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != true {
		t.Fatalf("paused correction: %v err=%v", r, err)
	}
	after, _, _ := readCoordinatorState(t, s)
	cards := preparedCards(after)
	if len(cards) != 1 || str(objectOrEmpty(cards[0]["proactive"]), "status") != "stale" {
		t.Fatalf("card not stale: %v", cards)
	}
	if strings.Contains(str(cards[0], "xml"), "合成材料") {
		t.Fatalf("old fact text remained in stale card: %s", str(cards[0], "xml"))
	}
	if basis := objectsOrEmpty(objectOrEmpty(cards[0]["proactive"])["basis"]); len(basis) != 0 {
		t.Fatalf("old basis remained: %v", basis)
	}

	writePolicyFile(t, dbPath, `{"proactive":true}`)
	if r, err := s.runProactiveTick("foreground"); err != nil {
		t.Fatalf("resume: %v err=%v", r, err)
	}
	resumed, _, _ := readCoordinatorState(t, s)
	for _, card := range preparedCards(resumed) {
		xml := str(card, "xml")
		if strings.Contains(xml, "合成材料") {
			t.Fatalf("old fact text revived: %s", xml)
		}
	}
}

func TestProactiveCrossDayExpirationWhilePaused(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	day1 := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, day1)
	state := schedulePlanState(day1)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["text"] = "我的版本：先看材料"
	prep["textMode"] = "user"
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	state["artifacts"] = []any{
		object{"id": "keep-me", "type": "note", "date": "2026-11-11", "title": "保留的独立卡片", "xml": "<card>keep</card>"},
	}
	seedCoordinatorState(t, s, state)
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}
	writePolicyFile(t, dbPath, `{"proactive":false}`)

	day2 := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, day2)
	r, err := s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != true {
		t.Fatalf("cross-day paused tick: %v err=%v", r, err)
	}
	after, _, _ := readCoordinatorState(t, s)
	cards := preparedCards(after)
	if len(cards) != 1 || str(objectOrEmpty(cards[0]["proactive"]), "status") != "stale" {
		t.Fatalf("expired card not stale: %v", cards)
	}
	xml := str(cards[0], "xml")
	for _, old := range []string{"12:00–13:00", "15:30–16:30", "提前了 60 分钟"} {
		if strings.Contains(xml, old) {
			t.Fatalf("expired system content %q remained: %s", old, xml)
		}
	}
	if basis := objectsOrEmpty(objectOrEmpty(cards[0]["proactive"])["basis"]); len(basis) != 0 {
		t.Fatalf("expired basis remained: %v", basis)
	}
	kept := false
	for _, artifact := range objectsOrEmpty(after["artifacts"]) {
		if str(artifact, "id") == "keep-me" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("unrelated independent card was deleted")
	}
	prep = objectOrEmpty(objectsOrEmpty(after["goals"])[0]["preparation"])
	if str(prep, "text") != "我的版本：先看材料" || str(prep, "textMode") != "user" {
		t.Fatalf("user-authored text was not retained: %v", prep)
	}
	again, err := s.runProactiveTick("foreground")
	if err != nil || again["made"] != 0 || again["changed"] != false {
		t.Fatalf("second cross-day maintenance: %v err=%v", again, err)
	}
}

func TestProactiveCommitFailureDoesNotConsumeState(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	before, revision, _ := readCoordinatorState(t, s)
	beforeCards := len(preparedCards(before))

	setCoordinatorHook(t, func(stage string) error {
		if stage == "before-commit" {
			return errors.New("synthetic commit failure")
		}
		return nil
	})
	if _, err := s.runProactiveTick("foreground"); err == nil {
		t.Fatal("commit failure was not reported")
	}
	after, afterRevision, _ := readCoordinatorState(t, s)
	if afterRevision != revision || len(preparedCards(after)) != beforeCards {
		t.Fatalf("failed commit changed durable state: revision %d->%d cards %d->%d",
			revision, afterRevision, beforeCards, len(preparedCards(after)))
	}
	status := readProactiveStatus(dbPath)
	if status.LastSuccessAt != "" {
		t.Fatalf("failed commit recorded a success: %+v", status)
	}

	setCoordinatorHook(t, nil)
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("retry after failure: %v err=%v", r, err)
	}
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 0 || r["changed"] != false {
		t.Fatalf("repeat after success: %v err=%v", r, err)
	}
}

func TestProactiveAfterCommitReplyLossIsIdempotent(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	setCoordinatorHook(t, func(stage string) error {
		if stage == "after-commit" {
			return errors.New("synthetic reply loss")
		}
		return nil
	})
	if _, err := s.runProactiveTick("foreground"); err == nil {
		t.Fatal("reply loss was not reported")
	}
	state, _, _ := readCoordinatorState(t, s)
	if cards := preparedCards(state); len(cards) != 1 {
		t.Fatalf("durable card after reply loss: %d", len(cards))
	}

	// Cross-process retry: close and reopen the same database.
	path := s.dbPath
	s.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	setCoordinatorHook(t, nil)
	r, err := reopened.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != false {
		t.Fatalf("retry after reply loss: %v err=%v", r, err)
	}
	state, _, _ = readCoordinatorState(t, reopened)
	if cards := preparedCards(state); len(cards) != 1 {
		t.Fatalf("card duplicated after retry: %d", len(cards))
	}
	_ = dbPath
}

func TestProactiveInterleavingPreservesUserEdit(t *testing.T) {
	s, _ := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setCoordinatorHook(t, func(stage string) error {
		if stage != "after-read" {
			return nil
		}
		blocked := false
		once.Do(func() { blocked = true; close(reached) })
		if blocked {
			<-release
		}
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := s.runProactiveTick("foreground")
		done <- err
	}()
	<-reached

	current, revision, snapshot := readCoordinatorState(t, s)
	goals := objectsOrEmpty(current["goals"])
	goals[0]["step"] = "用户手写步骤"
	current["goals"] = toAnyObjects(goals)
	if r := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": snapshot, "state": current}); r["ok"] != true {
		t.Fatalf("interleaved edit: %v", r)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("tick after interleaving: %v", err)
	}
	after, _, _ := readCoordinatorState(t, s)
	if step := str(objectsOrEmpty(after["goals"])[0], "step"); step != "用户手写步骤" {
		t.Fatalf("user edit was overwritten: %q", step)
	}
	if cards := preparedCards(after); len(cards) != 1 {
		t.Fatalf("cards after interleaving: %d", len(cards))
	}
}

func TestProactiveInterleavingWithdrawalIsNotRevived(t *testing.T) {
	s, _ := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setCoordinatorHook(t, func(stage string) error {
		if stage != "after-read" {
			return nil
		}
		blocked := false
		once.Do(func() { blocked = true; close(reached) })
		if blocked {
			<-release
		}
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := s.runProactiveTick("foreground")
		done <- err
	}()
	<-reached
	current, revision, snapshot := readCoordinatorState(t, s)
	current["facts"] = []any{}
	current["sources"] = []any{}
	if r := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": snapshot, "state": current}); r["ok"] != true {
		t.Fatalf("interleaved withdrawal: %v", r)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("tick after withdrawal: %v", err)
	}
	after, _, _ := readCoordinatorState(t, s)
	for _, card := range preparedCards(after) {
		if strings.Contains(str(card, "xml"), "合成材料") {
			t.Fatalf("withdrawn text revived: %s", str(card, "xml"))
		}
	}
}

func TestProactiveDuplicateWorkersKeepOneCard(t *testing.T) {
	s, _ := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.runProactiveTick("foreground")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("worker error: %v", err)
		}
	}
	state, _, _ := readCoordinatorState(t, s)
	if cards := preparedCards(state); len(cards) != 1 {
		t.Fatalf("duplicate workers produced %d cards", len(cards))
	}
}

func TestProactiveCorruptPolicyStillRunsMaintenance(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}
	writePolicyFile(t, dbPath, `{`)

	current, revision, snapshot := readCoordinatorState(t, s)
	current["facts"] = []any{}
	current["sources"] = []any{}
	if r := bridgeRequest(t, s, object{"op": "write", "revision": revision, "base": snapshot, "state": current}); r["ok"] != true {
		t.Fatalf("withdraw: %v", r)
	}
	r, err := s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != true {
		t.Fatalf("corrupt policy maintenance: %v err=%v", r, err)
	}
	after, _, _ := readCoordinatorState(t, s)
	if cards := preparedCards(after); len(cards) != 1 || str(objectOrEmpty(cards[0]["proactive"]), "status") != "stale" {
		t.Fatalf("corrupt policy did not maintain: %v", cards)
	}
}

func TestProactiveStatusIsNotADedupeCursor(t *testing.T) {
	s, dbPath := coordinatorStore(t)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	setCoordinatorClock(t, now)
	seedCoordinatorState(t, s, schedulePlanState(now))
	if r, err := s.runProactiveTick("foreground"); err != nil || r["made"] != 1 {
		t.Fatalf("setup: %v err=%v", r, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(dbPath), proactiveStatusFile)); err != nil {
		t.Fatal(err)
	}
	r, err := s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != false {
		t.Fatalf("missing status changed dedupe: %v err=%v", r, err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dbPath), proactiveStatusFile), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = s.runProactiveTick("foreground")
	if err != nil || r["made"] != 0 || r["changed"] != false {
		t.Fatalf("corrupt status changed dedupe: %v err=%v", r, err)
	}
}
