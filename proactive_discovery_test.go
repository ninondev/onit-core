package main

import (
	"strings"
	"testing"
	"time"
)

func discoveryState(now time.Time) object {
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	return object{
		"version": 2,
		"sources": []any{
			object{"id": "s1", "kind": "self", "origin": "note", "title": "合成资料", "text": "合成资料原文", "at": old},
		},
		"facts": []any{},
		"discoveries": []any{
			object{
				"id": "d1", "version": 1, "status": "proposed", "kind": "finding",
				"title": "Synthetic independent discovery", "body": "Synthetic body",
				"citations": []any{object{"id": "s1", "quote": "合成资料原文"}},
			},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "合成发现目标", "step": "核对发现", "unit": "分钟", "at": old,
				"approvedBasis":   []any{object{"kind": "discovery", "id": "d1", "version": 1}},
				"approvedMethods": []any{object{"text": "先核对这条发现"}},
				"entries":         []any{},
				"brief": object{
					"request":      "整理要核对的发现",
					"materialRefs": []any{object{"kind": "discovery", "id": "d1", "version": 1}},
				},
			},
		},
		"artifacts": []any{},
	}
}

func TestDiscoveryBasisResolvesByKind(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := discoveryState(now)
	// A fact with the same id must not satisfy a discovery reference.
	state["facts"] = []any{object{"id": "d1", "text": "Synthetic fact with same id", "quote": "Synthetic fact with same id", "confirmed": true, "version": 1}}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	for _, want := range []string{"Synthetic independent discovery", "顾问提出的判断", "是推断，不是原始记录"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("discovery card missing %q: %s", want, xml)
		}
	}
	for _, unwanted := range []string{"Synthetic fact with same id", "依据有变化", "本人确认"} {
		if strings.Contains(xml, unwanted) {
			t.Fatalf("discovery card wrongly contains %q: %s", unwanted, xml)
		}
	}
	basis := objectsOrEmpty(objectOrEmpty(made[0]["proactive"])["basis"])
	if len(basis) != 1 || str(basis[0], "kind") != "discovery" || int(number(basis[0]["version"])) != 1 {
		t.Fatalf("discovery basis provenance lost: %s", encode(made[0]))
	}
}

func TestDiscoveryVersionChangeUpdatesCard(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := discoveryState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	discoveries := objectsOrEmpty(state["discoveries"])
	discoveries[0]["version"] = 2
	discoveries[0]["title"] = "Synthetic corrected discovery"
	discoveries[0]["status"] = "corrected"
	state["discoveries"] = toAnyObjects(discoveries)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("version change made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "Synthetic corrected discovery") || !strings.Contains(xml, "本人更正的说法") {
		t.Fatalf("corrected discovery not used: %s", xml)
	}
	if strings.Contains(xml, "Synthetic independent discovery") {
		t.Fatalf("old discovery title kept as current: %s", xml)
	}
	if strings.Contains(xml, "是推断，不是原始记录") {
		t.Fatalf("corrected discovery still labeled as an unverified inference: %s", xml)
	}
	basis := objectsOrEmpty(objectOrEmpty(made[0]["proactive"])["basis"])
	if len(basis) != 1 || int(number(basis[0]["version"])) != 2 {
		t.Fatalf("basis version not updated: %s", encode(made[0]))
	}
}

func TestStaleDiscoveryStopsOldPreparation(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := discoveryState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	discoveries := objectsOrEmpty(state["discoveries"])
	discoveries[0]["version"] = 2
	discoveries[0]["status"] = "stale"
	state["discoveries"] = toAnyObjects(discoveries)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed {
		t.Fatalf("stale discovery made=%d changed=%v err=%v", len(made), changed, err)
	}
	for _, card := range objectsOrEmpty(state["artifacts"]) {
		if strings.Contains(str(card, "xml"), "Synthetic independent discovery") {
			t.Fatalf("stale discovery title still shown: %s", str(card, "xml"))
		}
	}
}
