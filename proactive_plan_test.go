package main

import (
	"strings"
	"testing"
	"time"
)

func planAt(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

func schedulePlanState(now time.Time) object {
	windowBegin := planAt(11, 12, 0)
	windowEnd := planAt(11, 17, 0)
	return object{
		"version": 2,
		"sources": []any{
			object{
				"id": "calA", "kind": "file", "origin": "device-calendar/primary",
				"title": "Alpha", "text": "合成日程 A",
				"calendar": object{"begin": planAt(11, 14, 0).UnixMilli(), "end": planAt(11, 15, 0).UnixMilli(), "allDay": false},
				"calendarChange": object{
					"kind": "moved", "at": now.UnixMilli(),
					"before": object{"begin": planAt(11, 15, 0).UnixMilli(), "end": planAt(11, 16, 0).UnixMilli(), "allDay": false},
					"after":  object{"begin": planAt(11, 14, 0).UnixMilli(), "end": planAt(11, 15, 0).UnixMilli(), "allDay": false},
				},
			},
			object{
				"id": "calB", "kind": "file", "origin": "device-calendar/primary",
				"title": "Bravo", "text": "合成日程 B",
				"calendar": object{"begin": planAt(11, 14, 30).UnixMilli(), "end": planAt(11, 15, 30).UnixMilli(), "allDay": false},
			},
		},
		"facts": []any{
			object{"id": "f1", "text": "合成材料", "quote": "合成材料", "confirmed": true, "version": 1, "at": now.Format(time.RFC3339)},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "合成目标", "step": "准备合成评审", "unit": "分钟",
				"at":              now.Format(time.RFC3339),
				"proposalId":      "synthetic-proposal",
				"approvedMethods": []any{object{"text": "先看材料"}},
				"approvedBasis":   []any{object{"kind": "fact", "id": "f1", "version": 1}},
				"entries":         []any{},
				"preparation": object{
					"window":       object{"begin": windowBegin.UnixMilli(), "end": windowEnd.UnixMilli()},
					"needMinutes":  60,
					"calendarIds":  []any{"calA", "calB"},
					"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
					"synthetic":    true,
				},
			},
		},
		"artifacts": []any{},
	}
}

func TestSchedulePlanComputesMovedCalendarOverlapAndSlots(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("made=%d changed=%v err=%v", len(made), changed, err)
	}
	card := made[0]
	proactive := objectOrEmpty(card["proactive"])
	if kind := str(proactive, "kind"); kind != "schedule-plan" {
		t.Fatalf("kind=%s", kind)
	}
	if status := str(proactive, "status"); status != "ready" {
		t.Fatalf("status=%s", status)
	}
	xml := str(card, "xml")
	for _, want := range []string{
		`kind="schedule-plan"`, `status="ready"`, `<test>合成测试</test>`,
		"提前了 60 分钟", "重叠 30 分钟", "12:00–13:00", "15:30–16:30",
		"本人确认", "先看材料", `<result kind="system">`, "其他占用未知",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("missing %q in %s", want, xml)
		}
	}
	seen := map[string]bool{}
	for _, ref := range objectsOrEmpty(proactive["basis"]) {
		seen[str(ref, "kind")+":"+str(ref, "id")] = true
	}
	for _, want := range []string{"source:calA", "source:calB", "fact:f1"} {
		if !seen[want] {
			t.Fatalf("missing basis %s in %v", want, seen)
		}
	}
}

func TestSchedulePlanDoesNotFabricateSlotsWhenWindowIsBusy(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	sources := objectsOrEmpty(state["sources"])
	for _, source := range sources {
		source["calendar"] = object{"begin": planAt(11, 12, 0).UnixMilli(), "end": planAt(11, 17, 0).UnixMilli(), "allDay": false}
		source["calendarChange"] = nil
	}
	state["sources"] = toAnyObjects(sources)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("fabricated a slot: %s", xml)
	}
	if !strings.Contains(xml, "没有找到 60 分钟空档") {
		t.Fatalf("missing honest no-solution text: %s", xml)
	}
	if !strings.Contains(xml, "要不要换一个准备窗口") {
		t.Fatalf("missing follow-up question: %s", xml)
	}
}

func TestSchedulePlanPendingWithoutWindow(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	delete(prep, "window")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	if status := str(objectOrEmpty(made[0]["proactive"]), "status"); status != "pending" {
		t.Fatalf("status=%s", status)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("pending card must not contain slots: %s", xml)
	}
	if !strings.Contains(xml, "这次准备安排在什么时间范围内？") {
		t.Fatalf("missing one clear question: %s", xml)
	}
}

func TestSchedulePlanPendingWithoutDuration(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	delete(prep, "needMinutes")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	if status := str(objectOrEmpty(made[0]["proactive"]), "status"); status != "pending" {
		t.Fatalf("status=%s", status)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("pending card must not contain slots: %s", xml)
	}
	if !strings.Contains(xml, "这次准备需要多长时间？") {
		t.Fatalf("missing duration question: %s", xml)
	}
}

func TestSchedulePlanPendingBecomesReadyAndBack(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	window := prep["window"]
	delete(prep, "window")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("pending made=%d err=%v", len(made), err)
	}
	if status := str(objectOrEmpty(made[0]["proactive"]), "status"); status != "pending" {
		t.Fatalf("pending status=%s", status)
	}
	prep["window"] = window
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("ready made=%d changed=%v err=%v", len(made), changed, err)
	}
	proactive := objectOrEmpty(made[0]["proactive"])
	if status := str(proactive, "status"); status != "ready" {
		t.Fatalf("ready status=%s", status)
	}
	if proactive["updated"] != true {
		t.Fatalf("ready card should be marked updated: %v", proactive)
	}
	if xml := str(made[0], "xml"); !strings.Contains(xml, "<slot ") {
		t.Fatalf("ready card has no slots: %s", xml)
	}
	delete(prep, "window")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, changed, err = RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("pending again made=%d changed=%v err=%v", len(made), changed, err)
	}
	proactive = objectOrEmpty(made[0]["proactive"])
	if status := str(proactive, "status"); status != "pending" {
		t.Fatalf("pending again status=%s", status)
	}
	if xml := str(made[0], "xml"); strings.Contains(xml, "<slot ") {
		t.Fatalf("pending card kept slots: %s", xml)
	}
}

func TestSchedulePlanPreservesUserTextAndRecomputes(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	sources := objectsOrEmpty(state["sources"])
	sources[0]["calendar"] = object{"begin": planAt(11, 15, 0).UnixMilli(), "end": planAt(11, 16, 0).UnixMilli(), "allDay": false}
	sources[0]["calendarChange"] = nil
	state["sources"] = toAnyObjects(sources)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["text"] = "我写的准备版本"
	prep["chosenSlot"] = object{"begin": planAt(11, 12, 0).UnixMilli(), "end": planAt(11, 13, 0).UnixMilli()}
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("first made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "我写的准备版本") || !strings.Contains(xml, `<result kind="user">`) {
		t.Fatalf("user text not used: %s", xml)
	}
	if !strings.Contains(xml, `<chosen begin="`) {
		t.Fatalf("chosen slot not kept: %s", xml)
	}
	sources = objectsOrEmpty(state["sources"])
	sources[0]["calendar"] = object{"begin": planAt(11, 14, 0).UnixMilli(), "end": planAt(11, 15, 0).UnixMilli(), "allDay": false}
	sources[0]["calendarChange"] = object{
		"kind": "moved", "at": now.UnixMilli(),
		"before": object{"begin": planAt(11, 15, 0).UnixMilli(), "end": planAt(11, 16, 0).UnixMilli(), "allDay": false},
		"after":  object{"begin": planAt(11, 14, 0).UnixMilli(), "end": planAt(11, 15, 0).UnixMilli(), "allDay": false},
	}
	state["sources"] = toAnyObjects(sources)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("second made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml = str(made[0], "xml")
	if !strings.Contains(xml, "我写的准备版本") {
		t.Fatalf("user text was overwritten: %s", xml)
	}
	if !strings.Contains(xml, "系统重算后的结果和你改过的版本不同") {
		t.Fatalf("missing actual difference: %s", xml)
	}
	if !strings.Contains(xml, "提前了 60 分钟") {
		t.Fatalf("missing moved-calendar difference: %s", xml)
	}
	if !strings.Contains(xml, "12:00–13:00") {
		t.Fatalf("chosen slot disappeared: %s", xml)
	}
}

func TestSchedulePlanCancelledCalendarInvalidatesAndCanReturn(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	sources := objectsOrEmpty(state["sources"])
	sources[0]["calendarChange"] = object{
		"kind": "cancelled", "at": now.UnixMilli(),
		"before": object{"begin": planAt(11, 15, 0).UnixMilli(), "end": planAt(11, 16, 0).UnixMilli(), "allDay": false},
		"after":  nil,
	}
	state["sources"] = toAnyObjects(sources)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 0 {
		t.Fatalf("cancel made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(objectsOrEmpty(state["artifacts"])[0], "xml")
	if !strings.Contains(xml, `status="stale"`) {
		t.Fatalf("cancelled plan should be stale: %s", xml)
	}
	if strings.Contains(xml, "<slot") || strings.Contains(xml, "12:00") || strings.Contains(xml, "合成材料") {
		t.Fatalf("stale plan kept derived content: %s", xml)
	}
	sources = objectsOrEmpty(state["sources"])
	sources[0]["calendarChange"] = nil
	state["sources"] = toAnyObjects(sources)
	made, changed, err = RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("restore made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml = str(made[0], "xml")
	if strings.Contains(xml, `status="stale"`) || !strings.Contains(xml, "<slot ") {
		t.Fatalf("restored plan not rebuilt: %s", xml)
	}
}

func TestSchedulePlanRepeatedTickDoesNotDuplicate(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("first made=%d err=%v", len(made), err)
	}
	made, changed, err := RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("second made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 1 {
		t.Fatalf("artifacts=%d", len(rows))
	}
}

func TestSchedulePlanMissingMaterialIsUnknownNotFabricated(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["materialRefs"] = []any{object{"kind": "fact", "id": "f-missing", "version": 1}}
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "合成材料") {
		t.Fatalf("fabricated a missing material: %s", xml)
	}
	if strings.Contains(xml, "f-missing") {
		t.Fatalf("missing ref id leaked into the card: %s", xml)
	}
	if !strings.Contains(xml, "有一项依据已经找不到") {
		t.Fatalf("missing unknown material notice: %s", xml)
	}
}

func TestSchedulePlanWithdrawnMaterialSanitizesCard(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	// A real withdrawal removes the material fact the plan cited.
	state["facts"] = []any{}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("withdraw made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	for _, leaked := range []string{"合成材料", "<materials>", "f1"} {
		if strings.Contains(xml, leaked) {
			t.Fatalf("withdrawn plan kept %q: %s", leaked, xml)
		}
	}
	if !strings.Contains(xml, "有一项依据已经找不到") {
		t.Fatalf("withdrawn plan did not ask for a replacement: %s", xml)
	}
	for _, ref := range objectsOrEmpty(objectOrEmpty(made[0]["proactive"])["basis"]) {
		if str(ref, "kind") == "fact" {
			t.Fatalf("withdrawn fact stayed in structured basis: %s", encode(ref))
		}
	}
	// Restoring the material lets the plan rebuild without the old quote.
	state["facts"] = schedulePlanState(now)["facts"]
	made, changed, err = RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("restore made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml = str(made[0], "xml")
	if !strings.Contains(xml, "<slot ") || !strings.Contains(xml, "合成材料") {
		t.Fatalf("restored plan not rebuilt: %s", xml)
	}
}

func TestSchedulePlanDoesNotInferRelationFromTitle(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	delete(prep, "calendarIds")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	sources := objectsOrEmpty(state["sources"])
	sources = append(sources, object{
		"id": "calC", "kind": "file", "origin": "device-calendar/primary",
		"title": "合成目标评审", "text": "合成日程 C",
		"calendar": object{"begin": now.Add(7 * 24 * time.Hour).UnixMilli(), "end": now.Add(7*24*time.Hour + time.Hour).UnixMilli(), "allDay": false},
	})
	state["sources"] = toAnyObjects(sources)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	if kind := str(objectOrEmpty(made[0]["proactive"]), "kind"); kind != "schedule-plan" {
		t.Fatalf("unexpected card kind=%s", kind)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("inferred a relation from a similar title: %s", xml)
	}
	if !strings.Contains(xml, "这次准备要参考哪几个日程？") {
		t.Fatalf("missing relation question: %s", xml)
	}
}

func TestSchedulePlanDoesNotTreatUnchangedSummaryAsUserText(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	summary := strings.TrimSpace(str(prep, "computedSummary"))
	if summary == "" {
		t.Fatal("missing computed summary")
	}
	// Simulate an older save that copied the unchanged system summary into text.
	prep["text"] = summary
	delete(prep, "textMode")
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	state["artifacts"] = []any{}
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, `<result kind="user">`) {
		t.Fatalf("unchanged system summary was laundered into user text: %s", xml)
	}
	if !strings.Contains(xml, `<result kind="system">`) {
		t.Fatalf("system result missing: %s", xml)
	}
}

func TestStalePlanRemovesSystemCopyAndKeepsUserNote(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	summary := strings.TrimSpace(str(prep, "computedSummary"))
	prep["text"] = summary + "我的备注。"
	prep["textMode"] = "user"
	prep["userNote"] = "我的备注。"
	prep["systemCopy"] = summary
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["plan"] = object{
		"text": summary + "我的备注。", "textMode": "user",
		"userNote": "我的备注。", "systemCopy": summary,
	}
	state["artifacts"] = toAnyObjects(artifacts)
	state["facts"] = []any{}
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("withdraw made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "合成材料") {
		t.Fatalf("withdrawn material survived: %s", xml)
	}
	if !strings.Contains(xml, "我的备注。") {
		t.Fatalf("user note was lost: %s", xml)
	}
	prep = objectOrEmpty(objectsOrEmpty(state["goals"])[0]["preparation"])
	if str(prep, "text") != "我的备注。" {
		t.Fatalf("preparation text was not sanitized: %s", encode(prep))
	}
	if _, ok := prep["systemCopy"]; ok {
		t.Fatalf("system copy survived withdrawal: %s", encode(prep))
	}
}

func TestSchedulePlanCorrectionShowsCurrentWordingAndMarksChange(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("setup made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	prep := objectOrEmpty(goals[0]["preparation"])
	prep["text"] = "我写的准备版本"
	prep["textMode"] = "user"
	prep["userNote"] = "我写的准备版本"
	goals[0]["preparation"] = prep
	state["goals"] = toAnyObjects(goals)
	facts := objectsOrEmpty(state["facts"])
	facts[0]["text"] = "更正后的材料"
	facts[0]["quote"] = "更正后的材料"
	facts[0]["version"] = 2
	state["facts"] = toAnyObjects(facts)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("correction made=%d changed=%v err=%v", len(made), changed, err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "更正后的材料") {
		t.Fatalf("current wording missing: %s", xml)
	}
	if strings.Contains(xml, "合成材料") {
		t.Fatalf("old wording survived correction: %s", xml)
	}
	if !strings.Contains(xml, "我写的准备版本") {
		t.Fatalf("user text was lost on correction: %s", xml)
	}
	if !strings.Contains(xml, "系统重算后的结果和你改过的版本不同") {
		t.Fatalf("change was not marked: %s", xml)
	}
}

func TestSchedulePlanNeverRecommendsPastSlots(t *testing.T) {
	run := func(now time.Time) object {
		state := schedulePlanState(now)
		made, _, err := RunProactive(state, now)
		if err != nil || len(made) != 1 {
			t.Fatalf("now=%s made=%d err=%v", now, len(made), err)
		}
		return made[0]
	}
	morning := run(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	xml := str(morning, "xml")
	for _, want := range []string{"12:00–13:00", "13:00–14:00", "15:30–16:30", "16:00–17:00"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("morning missing %s: %s", want, xml)
		}
	}

	afternoon := run(time.Date(2026, 11, 11, 13, 30, 0, 0, time.UTC))
	xml = str(afternoon, "xml")
	for _, gone := range []string{"12:00–13:00", "13:00–14:00"} {
		if strings.Contains(xml, gone) {
			t.Fatalf("13:30 recommended a past slot %s: %s", gone, xml)
		}
	}
	if !strings.Contains(xml, "15:30–16:30") || !strings.Contains(xml, "16:00–17:00") {
		t.Fatalf("13:30 lost the remaining slots: %s", xml)
	}
	if !strings.Contains(xml, "现在是 13:30") {
		t.Fatalf("13:30 did not state the current time: %s", xml)
	}

	late := run(time.Date(2026, 11, 11, 16, 15, 0, 0, time.UTC))
	xml = str(late, "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("16:15 recommended a slot shorter than the need: %s", xml)
	}
	if !strings.Contains(xml, "要不要换一个准备窗口") {
		t.Fatalf("16:15 did not ask for a new window: %s", xml)
	}

	for _, now := range []time.Time{
		time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	} {
		card := run(now)
		xml = str(card, "xml")
		if strings.Contains(xml, "<slot ") {
			t.Fatalf("now=%s recommended a slot after the window ended: %s", now, xml)
		}
		if !strings.Contains(xml, "已经过去") {
			t.Fatalf("now=%s did not state the window expired: %s", now, xml)
		}
		if !strings.Contains(xml, "换一个新的时间范围") {
			t.Fatalf("now=%s did not ask for a new window: %s", now, xml)
		}
		if str(objectOrEmpty(card["proactive"]), "status") != "pending" {
			t.Fatalf("now=%s status=%s", now, str(objectOrEmpty(card["proactive"]), "status"))
		}
	}
}

func TestSchedulePlanAnsweredCardInvalidatesWhenInputsChange(t *testing.T) {
	now := planAt(11, 9, 0)
	for _, mode := range []string{"source-text", "calendar", "method"} {
		t.Run(mode, func(t *testing.T) {
			state := schedulePlanState(now)
			made, _, err := RunProactive(state, now)
			if err != nil || len(made) != 1 {
				t.Fatalf("made=%d err=%v", len(made), err)
			}
			cardID := str(made[0], "id")
			made[0]["answered"] = now.Format(time.RFC3339)
			switch mode {
			case "source-text":
				source := findSource(objectsOrEmpty(state["sources"]), "calA")
				source["text"] = "合成日程 A 已改"
			case "calendar":
				source := findSource(objectsOrEmpty(state["sources"]), "calA")
				source["calendar"] = object{"begin": planAt(11, 12, 0).UnixMilli(), "end": planAt(11, 13, 0).UnixMilli()}
				source["calendarChange"] = nil
			case "method":
				goal := objectsOrEmpty(state["goals"])[0]
				goal["approvedMethods"] = []any{object{"text": "换一个做法"}}
			}
			_, changed, err := RunProactive(state, now)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			var got object
			for _, artifact := range objectsOrEmpty(state["artifacts"]) {
				if str(artifact, "id") == cardID {
					got = artifact
				}
			}
			if got == nil {
				t.Fatal("answered card disappeared")
			}
			if status := str(objectOrEmpty(got["proactive"]), "status"); status != "stale" {
				t.Fatalf("status=%s, want stale", status)
			}
			if xml := str(got, "xml"); strings.Contains(xml, "合成材料") || strings.Contains(xml, "先看材料") {
				t.Fatalf("stale card kept derived content: %s", xml)
			}
		})
	}
}

func TestSchedulePlanAsksWhenDueDateConflictsWithWindow(t *testing.T) {
	now := planAt(11, 9, 0)
	state := schedulePlanState(now)
	objectsOrEmpty(state["goals"])[0]["dueDate"] = "2026-11-10"
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	card := made[0]
	if status := str(objectOrEmpty(card["proactive"]), "status"); status != "pending" {
		t.Fatalf("status=%s, want pending when the window is after the due date", status)
	}
	xml := str(card, "xml")
	if strings.Contains(xml, "<slot ") {
		t.Fatalf("offered slots after the due date without asking: %s", xml)
	}
	if !strings.Contains(xml, "截止日期之后") {
		t.Fatalf("did not ask which constraint wins: %s", xml)
	}

	state = schedulePlanState(now)
	objectsOrEmpty(state["goals"])[0]["dueDate"] = "2026-11-20"
	made, _, err = RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("positive control made=%d err=%v", len(made), err)
	}
	card = made[0]
	if status := str(objectOrEmpty(card["proactive"]), "status"); status != "ready" {
		t.Fatalf("positive control status=%s, want ready", status)
	}
	if xml := str(card, "xml"); !strings.Contains(xml, "<slot ") {
		t.Fatalf("positive control lost its slots: %s", xml)
	}
}
