package main

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDatabaseWithOlderGoals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE);
		CREATE TABLE goals (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
			title TEXT NOT NULL, step TEXT NOT NULL, unit TEXT NOT NULL, created_at TEXT NOT NULL);
		INSERT INTO users VALUES ('person',X'01');
		INSERT INTO goals VALUES ('old','person','练琴','每天五分钟','分钟','2026-11-01T12:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	goals, err := s.Goals("person")
	if err != nil || len(goals) != 1 || goals[0].ID != "old" || goals[0].Step != "每天五分钟" || goals[0].ProposalID != "" || goals[0].Basis != nil {
		t.Fatalf("older goal changed: %+v, %v", goals, err)
	}
}

func storeUser(t testing.TB, s *Store) (string, string) {
	t.Helper()
	token, err := s.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.User(token)
	if err != nil {
		t.Fatal(err)
	}
	return u, token
}

func TestReadCorrectIndependentStatements(t *testing.T) {
	const body = "周三九点收木料，我原本想因此请一天假。"
	const delivery = "周三九点收木料"
	// An authorized read quotes the two independent statements separately.
	readings := []Finding{{Text: delivery, Quote: delivery}, {Text: "我原本想因此请一天假。", Quote: "我原本想因此请一天假。"}}
	path := filepath.Join(t.TempDir(), "independent.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := storeUser(t, s)
	src, err := s.Import(u, "self", "虚构手记", body, readings)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddReadings(u, src.ID, []Finding{{Text: "归属不明的解读", Quote: body}}); err != nil {
		t.Fatal(err)
	}
	facts, err := s.Portrait(u)
	if err != nil || len(facts) != 2 {
		t.Fatalf("facts: %+v, %v", facts, err)
	}
	var intention Memory
	for _, f := range facts {
		if len(f.Alternatives) != 0 {
			t.Fatalf("mixed sentence became an alternative: %+v", f)
		}
		if f.Quote == readings[1].Quote {
			intention = f
		}
	}
	if _, err = s.Correct(u, intention.ID, "当天照常上班。", intention.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.AddReadings(u, src.ID, readings); err != nil {
		t.Fatal(err)
	}
	facts, err = s.Portrait(u)
	if err != nil || len(facts) != 2 {
		t.Fatalf("reread changed the record: %+v, %v", facts, err)
	}
	deliveryKept := false
	for _, f := range facts {
		if f.Text == delivery {
			deliveryKept = true
		}
	}
	if !deliveryKept {
		t.Fatal("delivery reading lost on reread")
	}
	if err = s.Forget(u, intention.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.AddReadings(u, src.ID, readings); err != nil {
		t.Fatal(err)
	}
	facts, err = s.Portrait(u)
	if err != nil || len(facts) != 1 || facts[0].Text != delivery {
		t.Fatalf("reread: %+v, %v", facts, err)
	}
}

func TestStorePersistenceAndCorrections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notebook #1.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	u, token := storeUser(t, s)
	f := Finding{Text: "You use a loom on Sundays.", Quote: "I use a loom on Sundays."}
	body := "I use a loom on Sundays. I sketch on Mondays."
	src, err := s.Import(u, "self", "Weekend", body, []Finding{f})
	if err != nil {
		t.Fatal(err)
	}
	alt := Finding{Text: "Weaving has a place in your week.", Quote: "a loom on Sundays"}
	if err = s.AddReadings(u, src.ID, []Finding{alt, alt}); err != nil {
		t.Fatal(err)
	}
	items, err := s.Portrait(u)
	if err != nil || len(items) != 1 || len(items[0].Alternatives) != 1 {
		t.Fatalf("alternatives: %v %v", items, err)
	}
	baseline := items[0]
	if _, err = s.Correct(u, baseline.ID, alt.Text, baseline.Version-1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	m, err := s.Correct(u, baseline.ID, "I stopped weaving and study animation.", baseline.Version)
	if err != nil || !m.Confirmed {
		t.Fatalf("correction: %v", err)
	}
	if _, err = s.Correct(u, m.ID, "late writer", baseline.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil || bytes.Contains(onDisk, []byte(token)) {
		t.Fatal("token must not be stored in plaintext")
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.User(token)
	if err != nil || again != u {
		t.Fatalf("identity after reopening: %v", err)
	}
	reimport, err := s.Import(u, "self", "Weekend", body, []Finding{f})
	if err != nil || reimport.ID != src.ID {
		t.Fatalf("reimport: %v", err)
	}
	if err = s.AddReadings(u, src.ID, []Finding{alt}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		line := "Weather note " + strings.Repeat("x", i+1)
		if _, err = s.Import(u, "file", "Forecast", line, []Finding{{Text: line, Quote: line}}); err != nil {
			t.Fatal(err)
		}
	}
	var confirmed *Memory
	items, err = s.Portrait(u)
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if items[i].ID == m.ID {
			confirmed = &items[i]
		}
	}
	if confirmed == nil || confirmed.Text != m.Text || len(confirmed.Alternatives) != 0 || confirmed.Quote != f.Quote {
		t.Fatalf("correction after reopen: %+v", confirmed)
	}
	if confirmed.Version != m.Version {
		t.Fatalf("reimport replaced personally confirmed evidence: %+v", confirmed)
	}
}

func TestStoreSourceRemovalAndIsolation(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := storeUser(t, s)
	v, _ := storeUser(t, s)
	body := "星期三看书。星期六画画。星期天登山。"
	findings := []Finding{{"星期三看书", "星期三看书。"}, {"星期六画画", "星期六画画。"}, {"星期天登山", "星期天登山。"}}
	src, err := s.Import(u, "self", "安排", body, findings)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Import(v, "self", "安排", body, findings)
	if err != nil {
		t.Fatal(err)
	}
	if src.ID == other.ID {
		t.Fatal("sources shared across users")
	}
	all, err := s.Portrait(u)
	if err != nil || len(all) != 3 {
		t.Fatal(err)
	}
	keep, drop, forgot := all[0], all[1], all[2]
	if _, err = s.Source(v, src.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account read: %v", err)
	}
	if _, err = s.Correct(v, keep.ID, "跨账号", keep.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account correction: %v", err)
	}
	if err = s.Forget(v, keep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account forget: %v", err)
	}
	if err = s.AddReadings(v, src.ID, findings); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account analysis: %v", err)
	}
	if err = s.RemoveSource(v, src.ID, []string{keep.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account removal: %v", err)
	}
	if _, err = s.Correct(u, keep.ID, "星期四去公园", keep.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Correct(u, drop.ID, drop.Text, drop.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.Forget(u, forgot.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.AddReadings(u, src.ID, findings); err != nil {
		t.Fatal(err)
	}
	all, err = s.Portrait(u)
	if err != nil || len(all) != 2 {
		t.Fatal("forgotten evidence returned")
	}
	if err = s.RemoveSource(u, src.ID, []string{keep.ID, forgot.ID}); err != nil {
		t.Fatal(err)
	}
	all, err = s.Portrait(u)
	if err != nil || len(all) != 1 || all[0].SourceID != "" || all[0].Quote != "" || all[0].Text != "星期四去公园" {
		t.Fatalf("retention: %v %v", all, err)
	}
	sources, err := s.Sources(u)
	if err != nil || len(sources) != 0 {
		t.Fatal("source survived removal")
	}
	all, err = s.Portrait(v)
	if err != nil || len(all) != 3 {
		t.Fatal("other user's portrait changed")
	}
}

func TestStoreRejectsUnsupportedEvidenceAtomically(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := storeUser(t, s)
	valid := Finding{Text: "Runs at dawn", Quote: "I run at dawn."}
	bad := Finding{Text: "Made up", Quote: "I live in Rome."}
	if _, err = s.Import(u, "self", "Morning", valid.Quote, []Finding{valid, bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid quotation: %v", err)
	}
	sources, err := s.Sources(u)
	if err != nil || len(sources) != 0 {
		t.Fatal("partial source left behind")
	}
	src, err := s.Import(u, "self", "Morning", valid.Quote, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddReadings(u, src.ID, []Finding{valid, bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid batch: %v", err)
	}
	items, err := s.Portrait(u)
	if err != nil || len(items) != 0 {
		t.Fatal("partial model batch left behind")
	}
}

func TestStoreCorrectionSurvivesDuplicateImport(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := storeUser(t, s)
	f := Finding{"I like pottery.", "I like pottery."}
	if _, err = s.Import(u, "self", "First notes", f.Quote, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Portrait(u)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.Correct(u, rows[0].ID, "I quit ceramics and now paint.", rows[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(u, "file", "Another copy", f.Quote, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	items, err := s.Portrait(u)
	if err != nil || len(items) != 2 {
		t.Fatalf("expected the correction plus the new copy: %+v %v", items, err)
	}
	for _, it := range items {
		if it.ID == updated.ID && (it.Text != updated.Text || !it.Confirmed) {
			t.Fatal("duplicate import eclipsed the correction")
		}
	}
	sources, err := s.Sources(u)
	if err != nil || len(sources) != 2 || sources[0].Text != "" || sources[1].Text != "" {
		t.Fatal("source listing included private body")
	}
}

func TestStoreCrossSourceEvidenceGoalsAndReports(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := storeUser(t, s)
	note, err := s.ImportFrom(u, "self", "local-notes", "周记", "我每周三练习大提琴，想在年底给外婆拉一首曲子。", nil)
	if err != nil {
		t.Fatal(err)
	}
	mailA, err := s.ImportFrom(u, "mail", "account-a", "排练通知", "本周三下午琴房已预约。", nil)
	if err != nil {
		t.Fatal(err)
	}
	mailB, err := s.ImportFrom(u, "mail", "account-b", "排练通知", "本周三下午琴房已预约。", nil)
	if err != nil || mailA.ID == mailB.ID {
		t.Fatalf("same-title sources across accounts were not isolated: %v", err)
	}
	cross, err := s.AddCrossClaim(u, "你把周三下午留给外婆的祝寿大提琴曲", []EvidenceSpan{
		{SourceID: mailA.ID, Quote: "本周三下午琴房已预约。"},
		{SourceID: note.ID, Quote: "想在年底给外婆拉一首曲子。"},
	})
	if err != nil || len(cross.Evidence) != 2 {
		t.Fatalf("cross-source claim: %#v %v", cross, err)
	}
	prop, err := s.Propose(u, "练习祝寿曲", cross.ID)
	if err != nil || prop.Basis != cross.ID {
		t.Fatalf("proposal: %#v %v", prop, err)
	}
	// Removing mailA promotes the surviving evidence span on note instead of dropping the claim.
	if err = s.RemoveSource(u, mailA.ID, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.Portrait(u)
	if err != nil || len(items) != 1 || items[0].SourceID != note.ID || len(items[0].Evidence) != 1 {
		t.Fatalf("surviving evidence not promoted: %#v %v", items, err)
	}
	goal, err := s.AcceptProposal(u, prop.ID, "大提琴祝寿曲", "慢练弓法", "分钟")
	if err != nil {
		t.Fatal(err)
	}
	e1, err := s.RecordGoal(u, goal.ID, 45, "第一段合伴奏")
	if err != nil {
		t.Fatal(err)
	}
	mistake, err := s.RecordGoal(u, goal.ID, 55, "误触多记了一次")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.WeeklyReport(u, s.nowOrUTC())
	if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Count != 2 || rep.Rows[0].Amount != 100 {
		t.Fatalf("weekly report before correction: %#v %v", rep, err)
	}
	// Removing the mistaken entry updates the saved weekly report automatically.
	if err = s.ChangeGoalEntry(u, goal.ID, mistake.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	adjusted := 30.0
	noteText := "核对后是 30 分钟"
	if err = s.ChangeGoalEntry(u, goal.ID, e1.ID, &adjusted, &noteText); err != nil {
		t.Fatal(err)
	}
	reports, err := s.Reports(u)
	if err != nil || len(reports) != 1 || reports[0].Rows[0].Count != 1 || reports[0].Rows[0].Amount != 30 || reports[0].Rows[0].Notes[0] != noteText {
		t.Fatalf("weekly report did not sync after entry correction: %#v %v", reports, err)
	}
}

