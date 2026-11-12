package main

import (
	"strings"
	"testing"
	"time"
)

// 取代旧的 TestProgressInvalidatesNoFirstStepCard：「还没有第一步记录」
// 属于已删除的下一步提醒卡。新契约：材料快照是用户配置的成果，不是
// 按进度出现的提醒——记录了进展也不会把它撤下或复制。
func TestProgressDoesNotRetireConfiguredBrief(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := proactiveState(now)
	goals := objectsOrEmpty(state["goals"])
	goals[0]["entries"] = []any{}
	state["goals"] = toAnyObjects(goals)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	if xml := str(objectsOrEmpty(state["artifacts"])[0], "xml"); !strings.Contains(xml, "材料快照") {
		t.Fatalf("setup card should be a material snapshot: %s", xml)
	}
	goals = objectsOrEmpty(state["goals"])
	goals[0]["entries"] = []any{object{"id": "e2", "amount": 1.0, "note": "合成进度", "at": now.Format(time.RFC3339)}}
	state["goals"] = toAnyObjects(goals)
	made, changed, err := RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("progress made=%d changed=%v err=%v", len(made), changed, err)
	}
	for _, card := range objectsOrEmpty(state["artifacts"]) {
		xml := str(card, "xml")
		if strings.Contains(xml, `status="stale"`) {
			t.Fatalf("progress retired a configured deliverable: %s", xml)
		}
		if !strings.Contains(xml, "每天跟读十分钟") {
			t.Fatalf("card lost its material: %s", xml)
		}
	}
}

// 取代旧的 TestChangedGoalMethodUpdatesEventPreparation：事件准备卡已
// 删。保留的不变量挂到材料快照上：已保存的做法是快照的一部分，做法
// 变了快照原地重算，新做法出现、旧做法不再冒充当前做法。
func TestChangedGoalMethodUpdatesBrief(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := proactiveState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	goals[0]["approvedMethods"] = []any{object{"text": "更新后的合成做法"}}
	state["goals"] = toAnyObjects(goals)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("method change made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "更新后的合成做法") {
		t.Fatalf("brief did not update its saved method: %s", xml)
	}
	if strings.Contains(xml, "跟读一段") || strings.Contains(xml, "录下来") {
		t.Fatalf("brief kept the old method as current: %s", xml)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 1 {
		t.Fatalf("update should not add a second card: %d", len(rows))
	}
}

// 取代旧的 TestMovedEventOutsideWindowInvalidatesAndCanReturn：事件准备
// 的生成已删，恢复留给出借后的重新配置。保留的不变量：旧版本留下的
// event-prep 卡在日程挪出 48 小时窗口后被判失效，派生时间全部清掉。
func TestMovedEventMarksLegacyCardStale(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	begin := now.Add(time.Hour)
	state := object{
		"version": 2,
		"sources": []any{
			object{"id": "e1", "kind": "file", "origin": "device-calendar/primary", "title": "Synthetic review session", "text": "",
				"calendar": object{"begin": begin.UnixMilli(), "end": begin.Add(time.Hour).UnixMilli(), "allDay": false}},
		},
		"facts":  []any{},
		"goals":  []any{},
		"artifacts": []any{object{
			"id": "widget:2026-11-11:event-e1", "date": now.Format("2006-01-02"), "type": "prepared",
			"goalId": "", "xml": `<prepared kind="event-prep" status="ready"><slot>10:00</slot></prepared>`,
			"proactive": object{"kind": "event-prep", "key": "event:e1", "status": "ready", "begin": begin.UnixMilli(), "end": begin.Add(time.Hour).UnixMilli()},
		}},
	}
	if made, changed, err := RunProactive(state, now); err != nil || changed || len(made) != 0 {
		t.Fatalf("in-window event should keep its legacy card, made=%d changed=%v err=%v", len(made), changed, err)
	}
	sources := objectsOrEmpty(state["sources"])
	moved := now.Add(7 * 24 * time.Hour)
	sources[0]["calendar"] = object{"begin": moved.UnixMilli(), "end": moved.Add(time.Hour).UnixMilli(), "allDay": false}
	state["sources"] = toAnyObjects(sources)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 0 {
		t.Fatalf("move out made=%d changed=%v err=%v", len(made), changed, err)
	}
	for _, card := range objectsOrEmpty(state["artifacts"]) {
		xml := str(card, "xml")
		if !strings.Contains(xml, `status="stale"`) {
			t.Fatalf("moved event card should be stale: %s", xml)
		}
		if strings.Contains(xml, "10:00") {
			t.Fatalf("moved event card kept the old time: %s", xml)
		}
	}
	// Move it back: the generator is gone, so nothing rebuilds the card;
	// a fresh preparation needs explicit configuration again.
	sources = objectsOrEmpty(state["sources"])
	back := now.Add(time.Hour)
	sources[0]["calendar"] = object{"begin": back.UnixMilli(), "end": back.Add(time.Hour).UnixMilli(), "allDay": false}
	state["sources"] = toAnyObjects(sources)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 0 {
		t.Fatalf("move back should not fabricate a card, made=%d err=%v", len(made), err)
	}
}

func manyBriefGoalsState(now time.Time) object {
	state := proactiveState(now)
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	goals := objectsOrEmpty(state["goals"])
	for i := 2; i <= 5; i++ {
		goals = append(goals, object{
			"id": "g" + string(rune('0'+i)), "title": "合成目标" + string(rune('0'+i)), "step": "每天做一点", "unit": "分钟", "at": old,
			"approvedMethods": []any{object{"text": "合成做法" + string(rune('0'+i))}},
			"entries":         []any{},
			"brief": object{
				"request":      "整理合成材料" + string(rune('0'+i)),
				"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
			},
		})
	}
	state["goals"] = toAnyObjects(goals)
	return state
}

// 取代旧的 TestDailyLimitFiveGoalsReloadAndCrossDay：每日两卡的提醒预算
// 已删，多个目标各自配置的快照同批生成。保留的骨干不变量：重复运行、
// 重新加载（JSON 往返）、跨天都不产生新卡——快照是长期成果，不按天换。
func TestBriefsSurviveReloadAndCrossDay(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := manyBriefGoalsState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 5 {
		t.Fatalf("first tick made=%d err=%v", len(made), err)
	}
	for i := 0; i < 3; i++ {
		made, changed, err := RunProactive(state, now)
		if err != nil || changed || len(made) != 0 {
			t.Fatalf("tick %d made=%d changed=%v err=%v", i+2, len(made), changed, err)
		}
	}
	round, err := parseObject([]byte(encode(state)))
	if err != nil {
		t.Fatal(err)
	}
	if made, changed, err := RunProactive(round, now); err != nil || changed || len(made) != 0 {
		t.Fatalf("reload made=%d changed=%v err=%v", len(made), changed, err)
	}
	next := now.Add(24 * time.Hour)
	if made, changed, err := RunProactive(round, next); err != nil || changed || len(made) != 0 {
		t.Fatalf("cross-day made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(round["artifacts"]); len(rows) != 5 {
		t.Fatalf("artifacts=%d", len(rows))
	}
}

// 取代旧的 TestDailyLimitDeleteAndRestore：没有每日预算后，「删卡再补」
// 的不变量变成：快照卡被删掉后下一次 tick 重新生成（用户要的成果不能
// 丢）；把删除前的存档恢复回来则一切安静。
func TestDeletedBriefCardIsRegeneratedAndRestoreIsQuiet(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := manyBriefGoalsState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 5 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	kept := encode(state)
	artifacts := objectsOrEmpty(state["artifacts"])
	state["artifacts"] = toAnyObjects(artifacts[:1])
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 4 {
		t.Fatalf("after delete made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 5 {
		t.Fatalf("regeneration should restore five cards: %d", len(rows))
	}
	restored, err := parseObject([]byte(kept))
	if err != nil {
		t.Fatal(err)
	}
	if made, changed, err := RunProactive(restored, now); err != nil || changed || len(made) != 0 {
		t.Fatalf("restored made=%d changed=%v err=%v", len(made), changed, err)
	}
}
