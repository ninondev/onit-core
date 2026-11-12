package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestBriefSnapshot prints the actual user-visible result for a fixed input.
// It is a manual before/after evidence helper, not an assertion suite.
func TestBriefSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	long := strings.Repeat("这是一段与核对项无关的背景说明。", 40) +
		"注意：预算上限不超过 5000 元，且不包含差旅费。"
	state := object{
		"version": 2,
		"sources": []any{
			object{"id": "s1", "kind": "self", "origin": "note", "title": "合成来源甲", "text": long, "version": 1, "at": now.Format(time.RFC3339)},
		},
		"facts": []any{
			object{"id": "f1", "sourceId": "s1", "text": "评审预算上限是 5000 元。交付日期是 12 月 3 日。", "quote": "评审预算上限是 5000 元。交付日期是 12 月 3 日。", "confirmed": true, "version": 1, "at": now.Format(time.RFC3339)},
			object{"id": "f2", "sourceId": "s1", "text": "供应商说可以包含培训，但需要另加费用。", "quote": "供应商说可以包含培训，但需要另加费用。", "confirmed": false, "version": 1, "at": now.Format(time.RFC3339)},
		},
		"discoveries": []any{},
		"goals": []any{
			object{
				"id": "g1", "title": "合成目标甲", "step": "准备合成评审", "unit": "分钟",
				"at": now.Format(time.RFC3339), "approvedMethods": []any{object{"text": "先看材料"}},
				"approvedBasis": []any{}, "entries": []any{},
				"brief": object{
					"request": "整理成评审前能看的对照简报，重点核对：预算上限、交付日期、是否包含培训。",
					"materialRefs": []any{
						object{"kind": "fact", "id": "f1", "version": 1},
						object{"kind": "fact", "id": "f2", "version": 1},
						object{"kind": "source", "id": "s1", "version": 1},
					},
				},
			},
		},
		"artifacts": []any{},
	}
	goal := objectsOrEmpty(state["goals"])[0]
	card, ok := buildMaterialBrief(state, goal, objectsOrEmpty(state["sources"]), objectsOrEmpty(state["facts"]), objectsOrEmpty(state["discoveries"]), now, map[string]object{}, map[string]bool{})
	if !ok {
		t.Fatal("brief not built")
	}
	xml := str(card, "xml")
	match := regexp.MustCompile(`(?s)<result[^>]*>(.*?)</result>`).FindStringSubmatch(xml)
	if len(match) != 2 {
		t.Fatalf("no result in %s", xml)
	}
	fmt.Println("=== BRIEF RESULT START ===")
	fmt.Println(strings.ReplaceAll(match[1], "&quot;", `"`))
	fmt.Println("=== BRIEF RESULT END ===")
}

// TestBriefSnapshotUnanswered prints the second fixed input, where one of the
// user's check items has no matching sentence in the selected materials.
func TestBriefSnapshotUnanswered(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	goal := objectsOrEmpty(state["goals"])[0]
	brief := objectOrEmpty(goal["brief"])
	brief["request"] = "整理成评审前能看的对照简报，重点核对：预算上限、交付日期、违约责任。"
	goal["brief"] = brief
	state["goals"] = toAnyObjects([]object{goal})
	card, ok := buildMaterialBrief(state, goal, objectsOrEmpty(state["sources"]), objectsOrEmpty(state["facts"]), objectsOrEmpty(state["discoveries"]), now, map[string]object{}, map[string]bool{})
	if !ok {
		t.Fatal("brief not built")
	}
	xml := str(card, "xml")
	match := regexp.MustCompile(`(?s)<result[^>]*>(.*?)</result>`).FindStringSubmatch(xml)
	if len(match) != 2 {
		t.Fatalf("no result in %s", xml)
	}
	fmt.Println("=== BRIEF RESULT START ===")
	fmt.Println(strings.ReplaceAll(match[1], "&quot;", `"`))
	fmt.Println("=== BRIEF RESULT END ===")
}
