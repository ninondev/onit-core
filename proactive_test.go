package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 主动层现在只做两件事：维护旧卡失效（markStalePrepared），以及生成
// 用户显式配置的成果（日程准备、材料快照）。没有配置就没有卡；
// 理解、判断、建议都留给授权后的真实模型。
func proactiveState(now time.Time) object {
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	return object{
		"version": 2,
		"sources": []any{
			object{"id": "s1", "kind": "self", "origin": "note", "title": "手记", "text": "每天跟读十分钟。", "at": old},
		},
		"facts": []any{
			object{"id": "f1", "sourceId": "s1", "text": "每天跟读十分钟。", "quote": "每天跟读十分钟。", "confirmed": true, "version": 1},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "练口语", "step": "每天十分钟", "unit": "分钟", "at": old,
				"approvedBasis":   []any{object{"id": "f1", "version": 1}},
				"approvedMethods": []any{object{"text": "跟读一段"}, object{"text": "录下来"}},
				"entries":         []any{object{"id": "e1", "amount": 10.0, "note": "跟读", "at": old}},
				"brief": object{
					"request":      "把所选材料整理成快照",
					"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
				},
			},
		},
		"artifacts": []any{},
	}
}

// withoutBrief strips the configured brief, leaving a goal that asks for no
// prepared output at all.
func withoutBrief(state object) object {
	goals := objectsOrEmpty(state["goals"])
	delete(goals[0], "brief")
	state["goals"] = toAnyObjects(goals)
	return state
}

// 取代旧的 TestProactiveGoalNextStep：下一步卡是已删除的生成启发式。
// 保留的不变量：配置了 brief 的目标产一张材料快照卡，内容是请求原文、
// 材料原文和「未调用模型」的诚实声明；同一天重跑不重复。
func TestProactiveMaterialBriefCard(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("made=%d changed=%v err=%v", len(made), changed, err)
	}
	card := made[0]
	proactive := objectOrEmpty(card["proactive"])
	if str(card, "type") != "prepared" || str(card, "goalId") != "g1" || str(proactive, "kind") != "material-brief" {
		t.Fatalf("wrong card: %s", encode(card))
	}
	xml := str(card, "xml")
	for _, want := range []string{
		"材料快照", "「练口语」的材料快照", "把所选材料整理成快照", "每天跟读十分钟。",
		"已保存的做法（未执行）", "跟读一段", "尚未分析你的请求", `status="ready"`,
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("card xml missing %q: %s", want, xml)
		}
	}
	// 同一天再跑不重复。
	made, changed, err = RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("second run made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 1 {
		t.Fatalf("artifacts=%d", len(rows))
	}
}

// 取代旧的 TestProactiveSkipsFreshGoal：「今天练过就别再提」是已删除的
// 提醒语义。新的门槛只有配置本身：请求为空、材料为空或被忽略都不生成；
// 配置完整时，今天的记录不抑制快照——它是用户要的成果，不是打扰。
func TestProactiveSkipsUnconfiguredBrief(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	goals := objectsOrEmpty(state["goals"])
	goals[0]["entries"] = []any{object{"id": "e2", "amount": 1.0, "note": "今天做了", "at": now.Format(time.RFC3339)}}
	state["goals"] = toAnyObjects(goals)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("configured brief should build despite a fresh entry, made=%d err=%v", len(made), err)
	}
	missing := map[string]func(object){
		"empty-request": func(brief object) { brief["request"] = "" },
		"no-materials":  func(brief object) { brief["materialRefs"] = []any{} },
		"ignored":       func(brief object) { brief["ignored"] = true },
		"absent":        nil,
	}
	for name, mutate := range missing {
		state := proactiveState(now)
		goals := objectsOrEmpty(state["goals"])
		if mutate == nil {
			delete(goals[0], "brief")
		} else {
			brief := objectOrEmpty(goals[0]["brief"])
			mutate(brief)
			goals[0]["brief"] = brief
		}
		state["goals"] = toAnyObjects(goals)
		made, _, err := RunProactive(state, now)
		if err != nil || len(made) != 0 {
			t.Fatalf("%s should not build, made=%d err=%v", name, len(made), err)
		}
	}
}

func TestProactiveDismissalCooldown(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	state["proactive"] = object{"dismissed": []any{
		object{"key": "brief:g1", "until": now.Add(48 * time.Hour).Format(time.RFC3339), "reason": "user"},
	}}
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 0 {
		t.Fatalf("dismissed brief should stay quiet, made=%d err=%v", len(made), err)
	}
	state["proactive"] = object{"dismissed": []any{
		object{"key": "brief:g1", "until": now.Add(-time.Hour).Format(time.RFC3339), "reason": "user"},
	}}
	made, _, err = RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("expired dismissal should allow a card, made=%d err=%v", len(made), err)
	}
}

func TestProactiveBasisChangeUpdatesCard(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("first run made=%d err=%v", len(made), err)
	}
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["position"] = 5
	state["artifacts"] = toAnyObjects(artifacts)
	facts := objectsOrEmpty(state["facts"])
	facts[0]["version"] = 2
	facts[0]["text"] = "每天跟读十五分钟。"
	facts[0]["quote"] = "每天跟读十五分钟。"
	state["facts"] = toAnyObjects(facts)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("basis change made=%d changed=%v err=%v", len(made), changed, err)
	}
	card := made[0]
	xml := str(card, "xml")
	if !strings.Contains(xml, "每天跟读十五分钟") || strings.Contains(xml, "每天跟读十分钟") {
		t.Fatalf("updated card missing new basis text: %s", xml)
	}
	if objectOrEmpty(card["proactive"])["updated"] != true {
		t.Fatalf("updated flag missing: %s", encode(card["proactive"]))
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 1 || number(rows[0]["position"]) != 5 {
		t.Fatalf("basis change should update in place and keep position, artifacts=%s", encode(rows))
	}
}

// 取代旧的 TestProactiveEventPrepAndCancellation 前半段：日程准备卡的
// 生成启发式已删除。保留后半段的不变量：旧版本产出的 event-prep 卡
// 在日程取消后必须被维护逻辑判失效，不能继续以 ready 状态展示。
func TestProactiveCancelledEventMarksLegacyCardStale(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := withoutBrief(proactiveState(now))
	begin := now.Add(24 * time.Hour)
	end := begin.Add(time.Hour)
	sources := objectsOrEmpty(state["sources"])
	sources = append(sources, object{
		"id": "cal1", "kind": "file", "origin": "device-calendar/primary", "title": "口语练习课",
		"text": "", "calendar": object{"begin": begin.UnixMilli(), "end": end.UnixMilli(), "allDay": false},
	})
	state["sources"] = toAnyObjects(sources)
	state["artifacts"] = []any{object{
		"id": "widget:2026-11-11:event-cal1", "date": now.Format("2006-01-02"), "type": "prepared",
		"goalId": "", "xml": `<prepared kind="event-prep" status="ready"><title>口语练习课</title></prepared>`,
		"proactive": object{"kind": "event-prep", "key": "event:cal1", "status": "ready", "begin": begin.UnixMilli(), "end": end.UnixMilli()},
	}}
	made, changed, err := RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("future event should keep its legacy card, made=%d changed=%v err=%v", len(made), changed, err)
	}
	sources = objectsOrEmpty(state["sources"])
	sources[len(sources)-1]["calendarChange"] = object{"kind": "cancelled"}
	state["sources"] = toAnyObjects(sources)
	made, changed, err = RunProactive(state, now)
	if err != nil || !changed || len(made) != 0 {
		t.Fatalf("cancelled event made=%d changed=%v err=%v", len(made), changed, err)
	}
	for _, card := range objectsOrEmpty(state["artifacts"]) {
		if str(objectOrEmpty(card["proactive"]), "kind") == "event-prep" {
			if !strings.Contains(str(card, "xml"), `status="stale"`) {
				t.Fatalf("cancelled event card should be stale: %s", str(card, "xml"))
			}
			if strings.Contains(str(card, "xml"), "口语练习课") {
				t.Fatalf("stale card kept derived text: %s", str(card, "xml"))
			}
		}
	}
}

// 取代旧的 TestProactiveDailyLimitCountsExistingCards：每日两卡的提醒
// 预算属于已删除的启发式。新的不变量：每个目标自己配置的成果各自
// 生成，互不挤占，重复运行幂等。
func TestProactiveBuildsConfiguredBriefsPerGoal(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	goals := objectsOrEmpty(state["goals"])
	goals = append(goals, object{
		"id": "g2", "title": "读书", "step": "每天读十页", "unit": "页", "at": old,
		"approvedMethods": []any{object{"text": "读十页"}},
		"entries":         []any{object{"id": "e2", "amount": 5.0, "note": "读了几页", "at": old}},
		"brief": object{
			"request":      "整理读书材料",
			"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
		},
	})
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 2 {
		t.Fatalf("each configured goal should make its brief, made=%d err=%v", len(made), err)
	}
	made, changed, err := RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("repeat run made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 2 {
		t.Fatalf("artifacts=%d", len(rows))
	}
}

// 反关键词回归：日程标题和目标标题都用「口语练习课」，但目标没有配置
// 任何成果。旧引擎会靠词面关联生成日程准备卡；现在没有配置就没有卡。
func TestProactiveIgnoresUnrelatedEvent(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := withoutBrief(proactiveState(now))
	begin := now.Add(24 * time.Hour)
	state["sources"] = []any{object{
		"id": "cal1", "kind": "file", "origin": "device-calendar/primary", "title": "练口语",
		"text": "", "calendar": object{"begin": begin.UnixMilli(), "end": begin.Add(time.Hour).UnixMilli(), "allDay": false},
	}}
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 0 {
		t.Fatalf("unconfigured goal must not attach to an event by wording, made=%d err=%v", len(made), err)
	}
}

func TestProactiveLabelsUnconfirmedEvidence(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	facts := objectsOrEmpty(state["facts"])
	delete(facts[0], "sourceId")
	facts[0]["source"] = "对话记录"
	facts[0]["confirmed"] = false
	state["facts"] = toAnyObjects(facts)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if !strings.Contains(xml, "未确认记录") || !strings.Contains(xml, "还没有本人确认") {
		t.Fatalf("unconfirmed evidence should be labeled: %s", xml)
	}
	if strings.Contains(xml, "对话记录（未确认）") {
		t.Fatalf("label duplicated the qualifier: %s", xml)
	}
}

func TestProactiveEscapesCardText(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	goals := objectsOrEmpty(state["goals"])
	goals[0]["title"] = `<script>alert(1)</script>`
	state["goals"] = toAnyObjects(goals)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	if strings.Contains(xml, "<script>") || !strings.Contains(xml, "&lt;script&gt;") {
		t.Fatalf("card text not escaped: %s", xml)
	}
}

func TestProactiveAnsweredCardStaysQuiet(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("first run made=%d err=%v", len(made), err)
	}
	artifacts := objectsOrEmpty(state["artifacts"])
	artifacts[0]["answered"] = true
	state["artifacts"] = toAnyObjects(artifacts)
	made, changed, err := RunProactive(state, now)
	if err != nil || changed || len(made) != 0 {
		t.Fatalf("answered card should stay quiet, made=%d changed=%v err=%v", len(made), changed, err)
	}
	if rows := objectsOrEmpty(state["artifacts"]); len(rows) != 1 || rows[0]["answered"] != true {
		t.Fatalf("answered card was lost: %s", encode(rows))
	}
}

func TestProactiveGoalCompletionMarksCardStale(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	state := proactiveState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("first run made=%d err=%v", len(made), err)
	}
	goals := objectsOrEmpty(state["goals"])
	goals[0]["step"] = "已达成收尾"
	state["goals"] = toAnyObjects(goals)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 0 {
		t.Fatalf("completed goal made=%d changed=%v err=%v", len(made), changed, err)
	}
	rows := objectsOrEmpty(state["artifacts"])
	if len(rows) != 1 || !strings.Contains(str(rows[0], "xml"), `status="stale"`) {
		t.Fatalf("completed goal card should be stale: %s", encode(rows))
	}
}

func TestProactiveRestartDoesNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proactive.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state := proactiveState(now)
	write := object{"op": "write", "revision": 0, "state": state}
	if r := bridgeRequest(t, s, write); r["revision"] != 1 {
		t.Fatalf("write: %s", encode(r))
	}
	if r := bridgeRequest(t, s, object{"op": "tick"}); r["ok"] != true || number(r["made"]) != 1 {
		t.Fatalf("first tick: %s", encode(r))
	}
	s.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r := bridgeRequest(t, reopened, object{"op": "tick"})
	if r["ok"] != true || number(r["made"]) != 0 {
		t.Fatalf("tick after restart: %s", encode(r))
	}
	after, _ := r["state"].(object)
	if after == nil || len(objectsOrEmpty(after["artifacts"])) != 1 {
		t.Fatalf("card lost after restart: %s", encode(r))
	}
}

func TestDossierKeepsChatEvidence(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := object{
		"version": 2,
		"sources": []any{},
		"facts": []any{object{
			"id": "f1", "text": "每周三练一次口语", "quote": "每周三练一次口语",
			"source": "对话记录", "start": 1, "end": 9,
			"confirmed": false, "version": 1, "history": []any{},
			"evidence": []any{object{"sourceId": "chat:u1", "quote": "每周三练一次口语", "start": 1, "end": 9}},
			"category": "activity", "attribution": "self", "status": "occurred",
		}},
		"conversation": []any{object{"id": "u1", "role": "user", "text": "我每周三练一次口语。", "at": "2026-11-11T09:00:00Z"}},
		"goals":        []any{}, "proposals": []any{}, "reports": []any{},
		"discoveries": []any{}, "dismissed": []any{}, "artifacts": []any{},
	}
	if r := bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state}); r["revision"] != 1 {
		t.Fatalf("write: %s", encode(r))
	}
	read := bridgeRequest(t, s, object{"op": "read"})
	got, _ := read["state"].(object)
	facts := objectsOrEmpty(got["facts"])
	if len(facts) != 1 {
		t.Fatalf("facts=%s", encode(got["facts"]))
	}
	evidence := objectsOrEmpty(facts[0]["evidence"])
	if len(evidence) != 1 || str(evidence[0], "sourceId") != "chat:u1" {
		t.Fatalf("chat evidence lost: %s", encode(facts[0]))
	}
	if str(facts[0], "source") != "对话记录" {
		t.Fatalf("chat source label lost: %s", encode(facts[0]))
	}
}

func TestProactiveBridgeTickPersists(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	old := now.AddDate(0, 0, -4).Format(time.RFC3339)
	state := proactiveState(now)
	state["goals"] = []any{object{
		"id": "g1", "title": "练口语", "step": "每天十分钟", "unit": "分钟", "at": old,
		"approvedMethods": []any{object{"text": "跟读一段"}},
		"entries":         []any{object{"id": "e1", "amount": 10.0, "note": "跟读", "at": old}},
		"brief": object{
			"request":      "把所选材料整理成快照",
			"materialRefs": []any{object{"kind": "fact", "id": "f1", "version": 1}},
		},
	}}
	write := object{"op": "write", "revision": 0, "state": state}
	if r := bridgeRequest(t, s, write); r["revision"] != 1 {
		t.Fatalf("write: %s", encode(r))
	}
	r := bridgeRequest(t, s, object{"op": "tick"})
	if r["ok"] != true || number(r["made"]) != 1 || r["changed"] != true {
		t.Fatalf("tick: %s", encode(r))
	}
	after, _ := r["state"].(object)
	if after == nil || len(objectsOrEmpty(after["artifacts"])) != 1 {
		t.Fatalf("tick did not persist card: %s", encode(r))
	}
	// 第二次 tick 幂等。
	r = bridgeRequest(t, s, object{"op": "tick"})
	if r["ok"] != true || number(r["made"]) != 0 || r["changed"] != false {
		t.Fatalf("second tick: %s", encode(r))
	}
}
