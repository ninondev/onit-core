package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newPreparationStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "preparation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedPreparation creates one synthetic source, one fact from it, and one goal.
// The text is deliberately short so the rule-based brief can quote it whole.
func seedPreparation(t *testing.T, s *Store, user, text string) (sourceID, factID, goalID string) {
	t.Helper()
	src, err := s.Import(user, "self", "合成材料", text, []Finding{{Text: text, Quote: text}})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := s.Portrait(user)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range facts {
		if m.SourceID == src.ID {
			factID = m.ID
			break
		}
	}
	if factID == "" {
		t.Fatal("import did not create a fact")
	}
	goalID = "g-" + opaqueID(10)
	if _, err := s.db.Exec("INSERT INTO goals(id,user_id,title,step,unit,created_at) VALUES(?,?,?,?,?,?)",
		goalID, user, "整理合成材料", "收集材料", "条", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return src.ID, factID, goalID
}

func enablePreparation(t *testing.T, s *Store, user, clientKey, goalID, timezone string, refs ...preparationRef) preparationPolicy {
	t.Helper()
	policy, err := s.SavePreparationPolicy(user, clientKey, goalID, true, timezone, "把选中的材料整理成可核对的简报", refs)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func preparationOutputOf(t *testing.T, s *Store, user, clientKey string) preparationOutput {
	t.Helper()
	var out preparationOutput
	err := s.write(func(tx *sql.Tx) error {
		return readPreparationOutputTx(tx, user, clientKey, &out)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func preparationOutputExists(t *testing.T, s *Store, user, clientKey string) bool {
	t.Helper()
	var out preparationOutput
	err := s.write(func(tx *sql.Tx) error {
		return readPreparationOutputTx(tx, user, clientKey, &out)
	})
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func preparationRunOf(t *testing.T, s *Store, user, clientKey string) (string, int) {
	t.Helper()
	var result string
	var revision int
	if err := s.db.QueryRow("SELECT last_result,last_revision FROM preparation_runs WHERE user_id=? AND client_key=?", user, clientKey).Scan(&result, &revision); err != nil {
		t.Fatal(err)
	}
	return result, revision
}

func feedPreparation(t *testing.T, feed object, clientKey string) object {
	t.Helper()
	for _, row := range objectsOrEmpty(feed["preparations"]) {
		if str(row, "clientKey") == clientKey {
			return row
		}
	}
	t.Fatalf("preparation %q not in feed: %v", clientKey, feed["preparations"])
	return nil
}

func TestPreparationDefaultOffThenEnable(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "合成材料第一句。")
	refs := []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}}

	policy, err := s.SavePreparationPolicy(user, "shared-preparation-key", goal, false, "Asia/Shanghai", "整理材料", refs)
	if err != nil || policy.Enabled || policy.Version != 1 {
		t.Fatalf("default-off policy=%+v err=%v", policy, err)
	}
	if made, err := s.RunPreparationTick(user, time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)); err != nil || made != 0 {
		t.Fatalf("disabled tick made=%d err=%v", made, err)
	}
	if preparationOutputExists(t, s, user, "shared-preparation-key") {
		t.Fatal("disabled policy must not create an output")
	}
	feed, err := s.PreparationFeed(user, time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	row := feedPreparation(t, feed, "shared-preparation-key")
	if row["enabled"] != false || row["status"] != "" || row["revision"] != 0 {
		t.Fatalf("disabled feed row=%v", row)
	}

	enabled, err := s.SavePreparationPolicy(user, "shared-preparation-key", goal, true, "Asia/Shanghai", "整理材料", refs)
	if err != nil || !enabled.Enabled || enabled.Version != 2 {
		t.Fatalf("enable policy=%+v err=%v", enabled, err)
	}
	if made, err := s.RunPreparationTick(user, time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)); err != nil || made != 1 {
		t.Fatalf("enabled tick made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if out.Status != "ready" || out.Revision != 1 || !strings.Contains(out.XML, "合成材料第一句") {
		t.Fatalf("enabled output=%+v", out)
	}
}

func TestPreparationOwnerIsolationSameKey(t *testing.T) {
	s := newPreparationStore(t)
	alice, _ := storeUser(t, s)
	bob, _ := storeUser(t, s)
	aSrc, aFact, aGoal := seedPreparation(t, s, alice, "爱丽丝的合成材料。")
	bSrc, bFact, bGoal := seedPreparation(t, s, bob, "鲍勃的合成材料。")
	enablePreparation(t, s, alice, "shared-preparation-key", aGoal, "Asia/Shanghai", preparationRef{Kind: "source", ID: aSrc}, preparationRef{Kind: "fact", ID: aFact})
	enablePreparation(t, s, bob, "shared-preparation-key", bGoal, "America/Los_Angeles", preparationRef{Kind: "source", ID: bSrc}, preparationRef{Kind: "fact", ID: bFact})

	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(alice, now); err != nil || made != 1 {
		t.Fatalf("alice tick made=%d err=%v", made, err)
	}
	if made, err := s.RunPreparationTick(bob, now); err != nil || made != 1 {
		t.Fatalf("bob tick made=%d err=%v", made, err)
	}
	aOut := preparationOutputOf(t, s, alice, "shared-preparation-key")
	bOut := preparationOutputOf(t, s, bob, "shared-preparation-key")
	if !strings.Contains(aOut.XML, "爱丽丝") || strings.Contains(aOut.XML, "鲍勃") {
		t.Fatalf("alice output crossed owners: %s", aOut.XML)
	}
	if !strings.Contains(bOut.XML, "鲍勃") || strings.Contains(bOut.XML, "爱丽丝") {
		t.Fatalf("bob output crossed owners: %s", bOut.XML)
	}
	if aOut.Date != "2026-11-12" || bOut.Date != "2026-11-11" {
		t.Fatalf("owner timezone dates alice=%q bob=%q", aOut.Date, bOut.Date)
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM preparation_outputs").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("output rows=%d err=%v", rows, err)
	}
	aFeed, _ := s.PreparationFeed(alice, now)
	bFeed, _ := s.PreparationFeed(bob, now)
	if str(aFeed, "account") == str(bFeed, "account") {
		t.Fatal("owner account namespaces must differ")
	}
}

func TestPreparationForeignRefsRejected(t *testing.T) {
	s := newPreparationStore(t)
	alice, aToken := storeUser(t, s)
	bob, _ := storeUser(t, s)
	_, _, aGoal := seedPreparation(t, s, alice, "爱丽丝的材料。")
	bSrc, bFact, bGoal := seedPreparation(t, s, bob, "鲍勃的材料。")

	cases := []struct {
		name string
		goal string
		refs []preparationRef
	}{
		{"foreign goal", bGoal, []preparationRef{{Kind: "source", ID: bSrc}}},
		{"foreign fact", aGoal, []preparationRef{{Kind: "fact", ID: bFact}}},
		{"foreign source", aGoal, []preparationRef{{Kind: "source", ID: bSrc}}},
	}
	for _, tc := range cases {
		if _, err := s.SavePreparationPolicy(alice, "shared-preparation-key", tc.goal, true, "Asia/Shanghai", "整理", tc.refs); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: expected ErrNotFound, got %v", tc.name, err)
		}
	}
	keys, err := s.preparationKeys(alice)
	if err != nil || len(keys) != 0 {
		t.Fatalf("foreign refs must not create a policy: keys=%v err=%v", keys, err)
	}

	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	body := map[string]any{
		"goalId": bGoal, "enabled": true, "timezone": "Asia/Shanghai",
		"request": "整理", "selected": []map[string]string{{"kind": "source", "id": bSrc}},
	}
	apiCall(t, server.URL, aToken, "PUT", "/v1/preparation/shared-preparation-key", body, 404)
}

func TestPreparationManualTickOwnerOnly(t *testing.T) {
	s := newPreparationStore(t)
	alice, aToken := storeUser(t, s)
	bob, _ := storeUser(t, s)
	aSrc, aFact, aGoal := seedPreparation(t, s, alice, "爱丽丝的手动 tick。")
	bSrc, bFact, bGoal := seedPreparation(t, s, bob, "鲍勃的手动 tick。")
	enablePreparation(t, s, alice, "shared-preparation-key", aGoal, "Asia/Shanghai", preparationRef{Kind: "source", ID: aSrc}, preparationRef{Kind: "fact", ID: aFact})
	enablePreparation(t, s, bob, "shared-preparation-key", bGoal, "Asia/Shanghai", preparationRef{Kind: "source", ID: bSrc}, preparationRef{Kind: "fact", ID: bFact})

	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	apiCall(t, server.URL, aToken, "POST", "/v1/agent/tick", nil, 200)

	if !preparationOutputExists(t, s, alice, "shared-preparation-key") {
		t.Fatal("alice preparation missing")
	}
	if preparationOutputExists(t, s, bob, "shared-preparation-key") {
		t.Fatal("bob preparation must not be advanced by alice's manual tick")
	}
}

func TestPreparationArtifactConflictOwner(t *testing.T) {
	s := newPreparationStore(t)
	alice, _ := storeUser(t, s)
	bob, _ := storeUser(t, s)
	day := "2026-11-12"
	if _, err := s.db.Exec(artifactSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO artifacts(id,user_id,date,type,xml,at) VALUES(?,?,?,?,?,?)",
		"widget:conflict", bob, day, "nudge", "bob-owned", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	err := s.upsertArtifact(alice, Artifact{ID: "widget:conflict", Date: day, Type: "nudge", XML: "alice-owned"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected owner conflict, got %v", err)
	}
	var xml, owner string
	if err := s.db.QueryRow("SELECT xml,user_id FROM artifacts WHERE id=?", "widget:conflict").Scan(&xml, &owner); err != nil {
		t.Fatal(err)
	}
	if owner != bob || xml != "bob-owned" {
		t.Fatalf("artifact overwritten: owner=%s xml=%s", owner, xml)
	}
}

func TestPreparationPauseRace(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "暂停竞态的合成材料。")
	refs := []preparationRef{{Kind: "source", ID: src}, {Kind: "fact", ID: fact}}
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", refs...)
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setPreparationHook(func(stage string) error {
		if stage == "after-snapshot" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	defer setPreparationHook(nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.RunPreparationTick(user, now)
		done <- err
	}()
	<-blocked
	if _, err := s.SavePreparationPolicy(user, "shared-preparation-key", goal, false, "Asia/Shanghai", "整理材料", refs); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("pause race tick: %v", err)
	}
	// Pausing before the first commit may leave no output at all; that is
	// correct. What must never survive is a ready card with the old text.
	if preparationOutputExists(t, s, user, "shared-preparation-key") {
		out := preparationOutputOf(t, s, user, "shared-preparation-key")
		if out.Status != "stale" || strings.Contains(out.XML, "暂停竞态") {
			t.Fatalf("paused output=%+v", out)
		}
	}
	feed, _ := s.PreparationFeed(user, now)
	row := feedPreparation(t, feed, "shared-preparation-key")
	if row["enabled"] != false || row["status"] == "ready" {
		t.Fatalf("paused feed row=%v", row)
	}
}

func TestPreparationWithdrawRace(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, _, goal := seedPreparation(t, s, user, "撤回竞态的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setPreparationHook(func(stage string) error {
		if stage == "after-snapshot" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	defer setPreparationHook(nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.RunPreparationTick(user, now)
		done <- err
	}()
	<-blocked
	if _, err := s.db.Exec("DELETE FROM sources WHERE user_id=? AND id=?", user, src); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("withdraw race tick: %v", err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if strings.Contains(out.XML, "撤回竞态") {
		t.Fatalf("withdrawn source text survived: %s", out.XML)
	}
	if out.Status != "stale" && out.Status != "missing" {
		t.Fatalf("withdrawn output status=%q", out.Status)
	}
}

func TestPreparationCorrectionRace(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	_, fact, goal := seedPreparation(t, s, user, "旧事实内容甲")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setPreparationHook(func(stage string) error {
		if stage == "after-snapshot" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	defer setPreparationHook(nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.RunPreparationTick(user, now)
		done <- err
	}()
	<-blocked
	if _, err := s.Correct(user, fact, "新事实内容乙", 1); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("correction race tick: %v", err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if strings.Contains(out.XML, "旧事实内容甲") || !strings.Contains(out.XML, "新事实内容乙") {
		t.Fatalf("corrected fact not rebuilt: %s", out.XML)
	}
	if out.Status != "ready" || out.Revision < 1 {
		t.Fatalf("corrected output=%+v", out)
	}
}

func TestPreparationSourceHashRace(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, _, goal := seedPreparation(t, s, user, "旧资料内容甲")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setPreparationHook(func(stage string) error {
		if stage == "after-snapshot" {
			once.Do(func() {
				close(blocked)
				<-release
			})
		}
		return nil
	})
	defer setPreparationHook(nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.RunPreparationTick(user, now)
		done <- err
	}()
	<-blocked
	// No version column on sources: the body changes while the identity stays.
	if _, err := s.db.Exec("UPDATE sources SET body=? WHERE user_id=? AND id=?", "新资料内容乙", user, src); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("source hash race tick: %v", err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if strings.Contains(out.XML, "旧资料内容甲") || !strings.Contains(out.XML, "新资料内容乙") {
		t.Fatalf("source body change not rebuilt: %s", out.XML)
	}
}

func TestPreparationCommitFailureDoesNotConsumeFingerprint(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "提交失败的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src}, preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	setPreparationHook(func(stage string) error {
		if stage == "before-commit" {
			return errors.New("injected commit failure")
		}
		return nil
	})
	if _, err := s.RunPreparationTick(user, now); err == nil {
		t.Fatal("expected injected commit failure")
	}
	setPreparationHook(nil)

	if preparationOutputExists(t, s, user, "shared-preparation-key") {
		t.Fatal("failed transaction must not leave an output")
	}
	if result, _ := preparationRunOf(t, s, user, "shared-preparation-key"); result != "failed" {
		t.Fatalf("run result=%q", result)
	}
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("retry made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if out.Status != "ready" || out.Revision != 1 {
		t.Fatalf("retry output=%+v", out)
	}
}

func TestPreparationReplyLossIsIdempotent(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "回复丢失的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src}, preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	setPreparationHook(func(stage string) error {
		if stage == "after-commit" {
			return errors.New("injected reply loss")
		}
		return nil
	})
	if _, err := s.RunPreparationTick(user, now); err == nil {
		t.Fatal("expected injected reply loss")
	}
	setPreparationHook(nil)

	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if out.Status != "ready" || out.Revision != 1 {
		t.Fatalf("durable output after reply loss=%+v", out)
	}
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 0 {
		t.Fatalf("retry after reply loss made=%d err=%v", made, err)
	}
	again := preparationOutputOf(t, s, user, "shared-preparation-key")
	if again.Revision != 1 || again.Fingerprint != out.Fingerprint {
		t.Fatalf("reply loss duplicated output: %+v", again)
	}
}

func TestPreparationRestartAndDoubleWorker(t *testing.T) {
	file := filepath.Join(t.TempDir(), "preparation-restart.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "重启与双工的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src}, preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)

	var wg sync.WaitGroup
	results := make([]int, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.RunPreparationTick(user, now)
		}(i)
	}
	wg.Wait()
	total := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		total += results[i]
	}
	if total != 1 {
		t.Fatalf("double worker made=%d, want exactly one", total)
	}
	first := preparationOutputOf(t, s, user, "shared-preparation-key")
	if first.Revision != 1 {
		t.Fatalf("double worker revision=%d", first.Revision)
	}
	s.Close()

	reopened, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if made, err := reopened.RunPreparationTick(user, now); err != nil || made != 0 {
		t.Fatalf("restart tick made=%d err=%v", made, err)
	}
	after := preparationOutputOf(t, reopened, user, "shared-preparation-key")
	if after.Revision != 1 || after.Fingerprint != first.Fingerprint {
		t.Fatalf("restart duplicated output: %+v", after)
	}
}

func TestPreparationFeedInvalidationWithoutTick(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, _, goal := seedPreparation(t, s, user, "读取时立即失效的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("initial tick made=%d err=%v", made, err)
	}
	before := preparationOutputOf(t, s, user, "shared-preparation-key")
	if !strings.Contains(before.XML, "读取时立即失效") {
		t.Fatalf("initial output missing quote: %s", before.XML)
	}

	if _, err := s.db.Exec("DELETE FROM sources WHERE user_id=? AND id=?", user, src); err != nil {
		t.Fatal(err)
	}
	feed, err := s.PreparationFeed(user, now)
	if err != nil {
		t.Fatal(err)
	}
	row := feedPreparation(t, feed, "shared-preparation-key")
	if row["status"] != "stale" {
		t.Fatalf("feed did not invalidate immediately: %v", row)
	}
	if strings.Contains(str(row, "xml"), "读取时立即失效") {
		t.Fatalf("feed kept withdrawn text: %s", str(row, "xml"))
	}
	if row["revision"] != 2 {
		t.Fatalf("feed revision=%v, want 2", row["revision"])
	}

	// Pausing must not revive the old text either.
	if _, err := s.SavePreparationPolicy(user, "shared-preparation-key", goal, false, "Asia/Shanghai", "整理材料", []preparationRef{{Kind: "source", ID: src}}); err != nil {
		t.Fatal(err)
	}
	feed, err = s.PreparationFeed(user, now)
	if err != nil {
		t.Fatal(err)
	}
	row = feedPreparation(t, feed, "shared-preparation-key")
	if row["enabled"] != false || row["status"] != "stale" || strings.Contains(str(row, "xml"), "读取时立即失效") {
		t.Fatalf("paused feed revived text: %v", row)
	}
}

func TestPreparationNoModelCalls(t *testing.T) {
	s := newPreparationStore(t)
	user, token := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "不调用模型的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src}, preparationRef{Kind: "fact", ID: fact})

	var calls int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	defer model.Close()
	server := httptest.NewServer(newService(s, Model{Endpoint: model.URL, Name: "synthetic"}))
	defer server.Close()
	raw := apiCall(t, server.URL, token, "POST", "/v1/preparation/tick", nil, 200)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("preparation tick called the model %d times", calls)
	}
	if !strings.Contains(string(raw), "没有模型参与推断") {
		t.Fatalf("expected rule-based disclaimer: %s", raw)
	}
}

func TestPreparationUnreferencedMaterialAndMissing(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	_, _, goal := seedPreparation(t, s, user, "被引用的事实。")
	other, err := s.Import(user, "self", "另一份材料", "社区窑炉周六开放。", nil)
	if err != nil {
		t.Fatal(err)
	}
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: other.ID})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("unreferenced tick made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if !strings.Contains(out.XML, "社区窑炉") {
		t.Fatalf("selected unreferenced material was ignored: %s", out.XML)
	}

	if _, err := s.db.Exec("DELETE FROM sources WHERE user_id=? AND id=?", user, other.ID); err != nil {
		t.Fatal(err)
	}
	// First tick marks the old output stale; the second writes the missing notice.
	if _, err := s.RunPreparationTick(user, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunPreparationTick(user, now); err != nil {
		t.Fatal(err)
	}
	out = preparationOutputOf(t, s, user, "shared-preparation-key")
	if out.Status != "missing" || strings.Contains(out.XML, "社区窑炉") {
		t.Fatalf("missing material output=%+v", out)
	}
}

func TestPreparationClearTombstone(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "清除墓碑的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "source", ID: src}, preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("initial tick made=%d err=%v", made, err)
	}
	if err := s.ClearPreparation(user, "shared-preparation-key"); err != nil {
		t.Fatal(err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if !out.Deleted || out.Status != "stale" || strings.Contains(out.XML, "清除墓碑") {
		t.Fatalf("clear tombstone=%+v", out)
	}
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 0 {
		t.Fatalf("tick after clear made=%d err=%v", made, err)
	}
	after := preparationOutputOf(t, s, user, "shared-preparation-key")
	if !after.Deleted || after.Revision != out.Revision {
		t.Fatalf("clear revived output: %+v", after)
	}
	feed, _ := s.PreparationFeed(user, now)
	row := feedPreparation(t, feed, "shared-preparation-key")
	if row["deleted"] != true || row["enabled"] != false {
		t.Fatalf("feed clear row=%v", row)
	}
}

// 同一 owner/key 跨 delete/re-enable 的 revision 必须单调，否则客户端会把
// 重新启用后的合法结果当成旧版本丢掉。
func TestPreparationTombstoneRevisionMonotonic(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	_, fact, goal := seedPreparation(t, s, user, "墓碑后重新启用的合成材料。")
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("initial tick made=%d err=%v", made, err)
	}
	first := preparationOutputOf(t, s, user, "shared-preparation-key")
	if err := s.ClearPreparation(user, "shared-preparation-key"); err != nil {
		t.Fatal(err)
	}
	tombstone := preparationOutputOf(t, s, user, "shared-preparation-key")
	if tombstone.Revision <= first.Revision {
		t.Fatalf("tombstone revision=%d first=%d", tombstone.Revision, first.Revision)
	}
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "fact", ID: fact})
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("revive tick made=%d err=%v", made, err)
	}
	revived := preparationOutputOf(t, s, user, "shared-preparation-key")
	if revived.Deleted || revived.Status != "ready" {
		t.Fatalf("revived output=%+v", revived)
	}
	if revived.Revision <= tombstone.Revision {
		t.Fatalf("revision regressed: tombstone=%d revived=%d", tombstone.Revision, revived.Revision)
	}
}

// 只选事实时，事实自己的来源正文也要进入依赖复核；但未显式选择的整份来源
// 不能因此变成生成材料。
func TestPreparationFactSourceChangeInvalidates(t *testing.T) {
	s := newPreparationStore(t)
	user, _ := storeUser(t, s)
	src, fact, goal := seedPreparation(t, s, user, "OLD_SOURCE_QUOTE")
	if _, err := s.db.Exec("UPDATE sources SET body=? WHERE user_id=? AND id=?", "OLD_SOURCE_QUOTE 未选正文里的独有句子", user, src); err != nil {
		t.Fatal(err)
	}
	enablePreparation(t, s, user, "shared-preparation-key", goal, "Asia/Shanghai", preparationRef{Kind: "fact", ID: fact})
	now := time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC)
	if made, err := s.RunPreparationTick(user, now); err != nil || made != 1 {
		t.Fatalf("initial tick made=%d err=%v", made, err)
	}
	out := preparationOutputOf(t, s, user, "shared-preparation-key")
	if out.Status != "ready" || !strings.Contains(out.XML, "OLD_SOURCE_QUOTE") {
		t.Fatalf("fact-only brief=%+v", out)
	}
	if strings.Contains(out.XML, "未选正文里的独有句子") {
		t.Fatalf("unselected source body leaked into generation: %s", out.XML)
	}

	setPreparationHook(func(stage string) error {
		if stage == "after-snapshot" {
			_, err := s.db.Exec("UPDATE sources SET body=? WHERE user_id=? AND id=?", "NEW_SOURCE_WITHOUT_OLD_QUOTE", user, src)
			return err
		}
		return nil
	})
	defer setPreparationHook(nil)
	if _, err := s.RunPreparationTick(user, now); err != nil {
		t.Fatal(err)
	}
	changed := preparationOutputOf(t, s, user, "shared-preparation-key")
	if changed.Status == "ready" && strings.Contains(changed.XML, "OLD_SOURCE_QUOTE") {
		t.Fatalf("changed source still produced old quote: %+v", changed)
	}
	if changed.Status != "stale" {
		t.Fatalf("changed source status=%q", changed.Status)
	}
	if changed.Revision <= out.Revision {
		t.Fatalf("changed source revision=%d previous=%d", changed.Revision, out.Revision)
	}
	if _, err := s.RunPreparationTick(user, now); err != nil {
		t.Fatal(err)
	}
	missing := preparationOutputOf(t, s, user, "shared-preparation-key")
	if missing.Status != "missing" || strings.Contains(missing.XML, "OLD_SOURCE_QUOTE") {
		t.Fatalf("missing after source change=%+v", missing)
	}
	if missing.Revision <= changed.Revision {
		t.Fatalf("missing revision=%d stale=%d", missing.Revision, changed.Revision)
	}
}
