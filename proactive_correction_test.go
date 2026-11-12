package main

import (
	"strings"
	"testing"
	"time"
)

// A corrected fact keeps the original quote as history. The material snapshot
// must cite the current text, never present the stale original as the fact.
// (Rode the deleted next-step card before; now rides the material brief.)
func TestPreparedCardUsesCurrentFactTextNotOriginalQuote(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	state := object{
		"version": 2,
		"sources": []any{
			object{"id": "s1", "kind": "self", "origin": "note", "title": "合成展馆通知", "text": "合成展馆七点关门", "at": old},
		},
		"facts": []any{
			object{
				"id": "f1", "sourceId": "s1", "text": "合成展馆七点关门",
				"quote": "合成展馆六点关门", "confirmed": true, "version": 2,
				"history": []any{object{"text": "合成展馆六点关门", "at": old}},
			},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "合成展馆参观", "step": "确认闭馆时间", "unit": "分钟", "at": old,
				"approvedBasis":   []any{object{"kind": "fact", "id": "f1", "version": 2}},
				"approvedMethods": []any{object{"text": "查当前闭馆时间"}},
				"entries":         []any{},
				"brief": object{
					"request":      "整理参观前要确认的信息",
					"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 2}},
				},
			},
		},
		"artifacts": []any{},
	}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "合成展馆七点关门") {
		t.Fatalf("current text missing from prepared card: %s", xml)
	}
	if strings.Contains(xml, "合成展馆六点关门") {
		t.Fatalf("original quote must not be shown as current evidence: %s", xml)
	}
	facts := objectsOrEmpty(state["facts"])
	if str(facts[0], "quote") != "合成展馆六点关门" {
		t.Fatalf("original quote was overwritten: %s", encode(facts[0]))
	}
	if str(facts[0], "text") != "合成展馆七点关门" {
		t.Fatalf("current text changed unexpectedly: %s", encode(facts[0]))
	}
	basis := objectsOrEmpty(objectOrEmpty(made[0]["proactive"])["basis"])
	if len(basis) != 1 || str(basis[0], "id") != "f1" || int(number(basis[0]["version"])) != 2 {
		t.Fatalf("basis provenance lost: %s", encode(made[0]))
	}
}
