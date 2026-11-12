package main

import (
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const kilnConstraint = "瓷窑交流会最早十点半开始，最迟十一点到。"

func journal(paragraphs int) string {
	var note strings.Builder
	note.WriteString("📓\n")
	for i := 1; i <= paragraphs; i++ {
		fmt.Fprintf(&note, "我把第%d块小样迎着窗光看了一遍，拿铅笔记下右侧的颜色变化。\n", i)
	}
	note.WriteString("> " + kilnConstraint + "\n🪴\n")
	note.WriteString("我每周更换登录口令。\n")
	note.WriteString("她说：“我打算周日去海边。”\n")
	note.WriteString(kilnConstraint)
	return note.String()
}

func authoredSentences(paragraphs int) []string {
	out := make([]string, 0, paragraphs)
	for i := 1; i <= paragraphs; i++ {
		out = append(out, fmt.Sprintf("我把第%d块小样迎着窗光看了一遍，拿铅笔记下右侧的颜色变化。", i))
	}
	return out
}

// An authorized read quotes exact spans of the document it was given.
func journalFindings(paragraphs int) []Finding {
	findings := append([]Finding{}, quotesToFindings(authoredSentences(paragraphs))...)
	return append(findings,
		Finding{Text: "我每周更换登录口令。", Quote: "我每周更换登录口令。"},
		Finding{Text: "她说：“我打算周日去海边。”", Quote: "她说：“我打算周日去海边。”"},
		Finding{Text: kilnConstraint, Quote: kilnConstraint})
}

func quotesToFindings(quotes []string) []Finding {
	findings := make([]Finding, 0, len(quotes))
	for _, q := range quotes {
		findings = append(findings, Finding{Text: q, Quote: q})
	}
	return findings
}

func spansFor(body string, quotes []string) []ReadingSpan {
	spans := make([]ReadingSpan, 0, len(quotes))
	for _, q := range quotes {
		start := strings.Index(body, q)
		spans = append(spans, ReadingSpan{Finding: Finding{Text: q, Quote: q}, Start: start, End: start + len(q)})
	}
	return spans
}

// 导入不等于阅读：没有任何离线提取，内容进入记忆只能走本人授权的阅读，
// 且引用必须逐字命中原文。撤回的记忆不被迟到的重复阅读复活。
func TestLongAuthoredJournal(t *testing.T) {
	const paragraphs = 31
	body := journal(paragraphs)
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(newService(store, Model{}))
	defer server.Close()
	user, token := storeUser(t, store)
	call := func(method, path string, data any, status int) []byte {
		return apiCall(t, server.URL, token, method, path, data, status)
	}
	src := unpack[Source](t, call("POST", "/v1/sources", object{"kind": "self", "origin": "note", "title": "釉色笔记", "text": body}, 200))
	if memories := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200)); len(memories) != 0 {
		t.Fatalf("import extracted memories without an authorized read: %d", len(memories))
	}
	findings := journalFindings(paragraphs)
	if err = store.AddReadings(user, src.ID, findings); err != nil {
		t.Fatal(err)
	}
	memories := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
	if len(memories) != len(findings) {
		t.Fatalf("lost readings, want %d got %d", len(findings), len(memories))
	}
	for _, m := range memories {
		// Evidence points at the authored occurrence: the same words may appear
		// earlier inside a quoted line, which belongs to someone else.
		start := authoredOccurrence(body, m.Quote)
		e := m.Evidence[0]
		if start < 0 || e.Start != start || e.End != start+len(m.Quote) || body[e.Start:e.End] != m.Quote || e.SourceID != src.ID {
			t.Fatal("evidence differs", m)
		}
	}
	// A quote that is not in the document is rejected, not located approximately.
	if err = store.AddReadings(user, src.ID, []Finding{{Text: "不存在的句子", Quote: "原文里没有这句话。"}}); err == nil {
		t.Fatal("accepted a quote that is not in the source")
	}
	var withdrawn string
	for _, m := range memories {
		if m.Quote == kilnConstraint {
			withdrawn = m.ID
		}
	}
	if withdrawn == "" {
		t.Fatal("constraint reading missing")
	}
	call("DELETE", "/v1/memories/"+withdrawn, nil, 204)
	if err = store.AddReadings(user, src.ID, findings); err != nil {
		t.Fatal(err)
	}
	memories = unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
	count := 0
	for _, m := range memories {
		if m.Quote == kilnConstraint {
			count++
		}
	}
	if count != 0 || len(memories) != len(findings)-1 {
		t.Fatalf("withdrawn reading revived or duplicated: %d memories, %d constraints", len(memories), count)
	}
	call("DELETE", "/v1/sources/"+src.ID, nil, 204)
	if got := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200)); len(got) != 0 {
		t.Fatal("source deletion left evidence", got)
	}
}

// 旧库迁移只登记处理状态：未读过的旧手记内容不因迁移进入记忆，撤回与更正
// 不被复活，重复迁移幂等，删除来源仍级联清理。
func TestUpgradeCompletedAuthoredReadings(t *testing.T) {
	for _, android := range []bool{false, true} {
		t.Run(fmt.Sprintf("android=%t", android), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "old.sqlite")
			s, err := OpenStore(file)
			if err != nil {
				t.Fatal(err)
			}
			user, _ := storeUser(t, s)
			if android {
				bridgeRequest(t, s, object{"op": "read"})
				if err = s.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&user); err != nil {
					t.Fatal(err)
				}
			}
			const forgotten = "我每月去剧场看演出。"
			const outdated = "我习惯在周日练吉他。"
			const current = "我想把周日留给家人。"
			const paragraphs = 7
			body := journal(paragraphs)
			// Simulate a legacy partial read: only the first six sentences were read.
			spans := spansFor(body, authoredSentences(paragraphs)[:6])
			src, err := s.storeSpans(user, "self", "note", "旧手记", body, spans, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Import(user, "self", "另一份记录", forgotten+"\n"+outdated, quotesToFindings([]string{forgotten, outdated})); err != nil {
				t.Fatal(err)
			}
			before, err := s.Portrait(user)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range before {
				switch m.Text {
				case spans[0].Text, forgotten:
					err = s.Forget(user, m.ID)
				case spans[1].Text, outdated:
					_, err = s.Correct(user, m.ID, current, m.Version)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			// The owner separately authorized one quote of the same note; that
			// is not authorization to read the rest of it.
			if _, err = s.Import(user, "self", "只选了其中一句", body, []Finding{{Text: kilnConstraint, Quote: kilnConstraint}}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.Portrait(user)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = OpenStore(file)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			recover := func() {
				if android {
					bridgeRequest(t, s, object{"op": "read", "completeReadings": true})
				} else if err := s.completeStoredReadings(""); err != nil {
					t.Fatal(err)
				}
			}
			if android {
				// Legacy imports must stay untouched until Java verifies the copy.
				bridgeRequest(t, s, object{"op": "read"})
				rows, err := s.Portrait(user)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != len(snapshot) {
					t.Fatal("modified unverified migration", rows)
				}
			}
			recover()
			recover()
			facts, err := s.Portrait(user)
			if err != nil {
				t.Fatal(err)
			}
			noteRows := 0
			for _, m := range facts {
				switch m.Text {
				case forgotten, outdated, spans[0].Text, spans[1].Text:
					t.Fatal("revived withdrawn memory", m)
				case kilnConstraint:
					// Only the explicitly authorized copy is present, on its own source.
					if m.SourceID == src.ID {
						t.Fatal("migration expanded an unauthorized reading", m)
					}
					noteRows++
				}
			}
			// The unread seventh sentence never entered memory.
			for _, m := range facts {
				if m.Quote == authoredSentences(paragraphs)[6] {
					t.Fatal("migration read unread content", m)
				}
			}
			if noteRows != 1 {
				t.Fatalf("wanted exactly the authorized reading, got %d", noteRows)
			}
			if err = s.RemoveSource(user, src.ID, nil); err != nil {
				t.Fatal(err)
			}
			recover()
			facts, err = s.Portrait(user)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range facts {
				if m.SourceID == src.ID {
					t.Fatal("source deletion left evidence", m)
				}
			}
		})
	}
}

func BenchmarkLongAuthoredJournal(b *testing.B) {
	const paragraphs = 3150
	body := journal(paragraphs)
	if utf8.RuneCountInString(body) > 120000 {
		b.Fatal("fixture exceeds the note limit")
	}
	b.Run("StoreImportAndAuthorizedReadings", func(b *testing.B) {
		s, err := OpenStore(":memory:")
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		findings := journalFindings(paragraphs)
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for b.Loop() {
			user, _ := storeUser(b, s)
			if _, err = s.importMaterial(user, "self", "note", "釉色笔记", body); err != nil {
				b.Fatal(err)
			}
			srcs, err := s.Sources(user)
			if err != nil || len(srcs) == 0 {
				b.Fatal(err)
			}
			if err = s.AddReadings(user, srcs[0].ID, findings); err != nil {
				b.Fatal(err)
			}
		}
	})
}
