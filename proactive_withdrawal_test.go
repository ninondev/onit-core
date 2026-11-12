package main

import (
	"strings"
	"testing"
	"time"
)

// Withdrawing a fact/source must remove the derived private quote from every
// prepared card that cited it, including yesterday's and answered cards. An
// unrelated card must keep its own evidence.
func TestWithdrawnBasisSanitizesEveryDerivedCard(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	yesterday := now.Add(-24 * time.Hour)
	old := now.AddDate(0, 0, -5).Format(time.RFC3339)
	state := object{
		"version": 2,
		"sources": []any{
			object{"id": "s1", "kind": "self", "origin": "note", "title": "合成私密来源", "text": "合成撤回标记-S1", "at": old},
			object{"id": "s2", "kind": "self", "origin": "note", "title": "合成保留来源", "text": "合成保留标记-S2", "at": old},
		},
		"facts": []any{
			object{"id": "f1", "sourceId": "s1", "text": "合成撤回标记-S1", "quote": "合成撤回标记-S1", "confirmed": true, "version": 1},
			object{"id": "f2", "sourceId": "s2", "text": "合成保留标记-S2", "quote": "合成保留标记-S2", "confirmed": true, "version": 1},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "合成撤回目标", "step": "核对撤回资料", "unit": "分钟", "at": old,
				"approvedBasis":   []any{object{"kind": "fact", "id": "f1", "version": 1}},
				"approvedMethods": []any{object{"text": "先看撤回资料"}},
				"entries":         []any{},
				"brief": object{
					"request":      "整理撤回资料",
					"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
				},
			},
			object{
				"id": "g2", "title": "合成保留目标", "step": "核对保留资料", "unit": "分钟", "at": old,
				"approvedBasis":   []any{object{"kind": "fact", "id": "f2", "version": 1}},
				"approvedMethods": []any{object{"text": "先看保留资料"}},
				"entries":         []any{},
				"brief": object{
					"request":      "整理保留资料",
					"materialRefs": []any{object{"kind": "fact", "id": "f2", "version": 1}},
				},
			},
		},
		"artifacts": []any{},
	}
	made, _, err := RunProactive(state, yesterday)
	if err != nil || len(made) != 2 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	artifacts := objectsOrEmpty(state["artifacts"])
	for _, card := range artifacts {
		if str(card, "goalId") == "g1" {
			card["answered"] = yesterday.Format(time.RFC3339)
		}
	}
	state["artifacts"] = toAnyObjects(artifacts)

	// A real withdrawal removes the fact and its source, not just the label.
	state["facts"] = []any{state["facts"].([]any)[1]}
	state["sources"] = []any{state["sources"].([]any)[1]}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed {
		t.Fatalf("withdrawal made=%d changed=%v err=%v", len(made), changed, err)
	}
	today := now.Format("2006-01-02")
	for _, card := range objectsOrEmpty(state["artifacts"]) {
		xml := str(card, "xml")
		if str(card, "goalId") == "g1" {
			if str(card, "date") != today {
				// Yesterday's card is the retained derived preparation. It
				// must be stale and must not keep the private quote.
				if !strings.Contains(xml, `status="stale"`) {
					t.Fatalf("withdrawn card not stale: %s", xml)
				}
				for _, leaked := range []string{"合成撤回标记-S1", "<evidence>", "<steps>", "<choices>"} {
					if strings.Contains(xml, leaked) {
						t.Fatalf("withdrawn card kept %q: %s", leaked, xml)
					}
				}
			} else if strings.Contains(xml, "合成撤回标记-S1") {
				t.Fatalf("new preparation cited a withdrawn fact: %s", xml)
			}
		}
		if str(card, "goalId") == "g2" {
			if strings.Contains(xml, `status="stale"`) {
				t.Fatalf("unrelated card was invalidated: %s", xml)
			}
			if !strings.Contains(xml, "合成保留标记-S2") {
				t.Fatalf("unrelated card lost its evidence: %s", xml)
			}
		}
	}
}
