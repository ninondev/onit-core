package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func briefState(now time.Time) object {
	return object{
		"version": 2,
		"sources": []any{
			object{
				"id": "s1", "kind": "self", "origin": "note",
				"title": "合成来源甲", "text": "合成来源甲里有一段可以引用的原文。",
				"version": 1, "at": now.Format(time.RFC3339),
			},
		},
		"facts": []any{
			object{"id": "f1", "sourceId": "s1", "text": "合成私密标记甲", "quote": "合成私密标记甲", "confirmed": true, "version": 1, "at": now.Format(time.RFC3339)},
			object{"id": "f2", "sourceId": "s1", "text": "合成私密标记乙", "quote": "合成私密标记乙", "confirmed": false, "version": 1, "at": now.Format(time.RFC3339)},
			object{"id": "f3", "text": "没有勾选的私密标记丙", "quote": "没有勾选的私密标记丙", "confirmed": true, "version": 1, "at": now.Format(time.RFC3339)},
		},
		"discoveries": []any{
			object{"id": "d1", "title": "合成推断丁", "status": "open", "version": 1, "at": now.Format(time.RFC3339)},
		},
		"goals": []any{
			object{
				"id": "g1", "title": "合成目标甲", "step": "准备合成评审", "unit": "分钟",
				"at":              now.Format(time.RFC3339),
				"proposalId":      "synthetic-proposal",
				"approvedMethods": []any{object{"text": "先看材料"}},
				"approvedBasis":   []any{object{"kind": "fact", "id": "f3", "version": 1}},
				"entries":         []any{},
				"brief": object{
					"request": "把材料整理成评审前能看的对照简报",
					"materialRefs": []any{
						object{"kind": "fact", "id": "f1", "version": 1},
						object{"kind": "fact", "id": "f2", "version": 1},
						object{"kind": "discovery", "id": "d1", "version": 1},
						object{"kind": "source", "id": "s1", "version": 1},
					},
					"synthetic": true,
				},
			},
		},
		"artifacts": []any{},
	}
}

func TestMaterialBriefBuildsWithoutCalendarAndUsesOnlySelectedMaterials(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("made=%d changed=%v err=%v", len(made), changed, err)
	}
	card := made[0]
	proactive := objectOrEmpty(card["proactive"])
	if kind := str(proactive, "kind"); kind != "material-brief" {
		t.Fatalf("kind=%s", kind)
	}
	if status := str(proactive, "status"); status != "ready" {
		t.Fatalf("status=%s", status)
	}
	xml := str(card, "xml")
	for _, want := range []string{
		`kind="material-brief"`, `status="ready"`, "材料快照", "请求原文：把材料整理成评审前能看的对照简报",
		"合成私密标记甲", "合成私密标记乙", "合成推断丁", "合成来源甲里有一段可以引用的原文",
		"本人确认", "未确认记录", "推断", "原始资料", "先看材料",
		"还没有本人确认", "是推断，不是原始记录",
		`<result kind="system">`, "未调用模型", "尚未分析请求",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("missing %q in %s", want, xml)
		}
	}
	if strings.Contains(xml, "没有勾选的私密标记丙") {
		t.Fatalf("unselected material leaked into the brief: %s", xml)
	}
	if strings.Contains(xml, "<window") || strings.Contains(xml, "<need") {
		t.Fatalf("brief should not require a time arrangement: %s", xml)
	}
	seen := map[string]bool{}
	for _, ref := range objectsOrEmpty(proactive["basis"]) {
		seen[str(ref, "kind")+":"+str(ref, "id")] = true
	}
	for _, want := range []string{"fact:f1", "fact:f2", "discovery:d1", "source:s1"} {
		if !seen[want] {
			t.Fatalf("missing basis %s in %v", want, seen)
		}
	}
	if seen["fact:f3"] {
		t.Fatalf("unselected basis f3 was added: %v", seen)
	}
}

func TestMaterialBriefPartialRemovalRebuildsWithoutWithdrawnText(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	first, _, err := RunProactive(state, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first made=%d err=%v", len(first), err)
	}
	cardID := str(first[0], "id")
	state["facts"] = toAnyObjects(objectsOrEmpty(state["facts"])[:1]) // keep f1, remove f2 and f3
	second, changed, err := RunProactive(state, now.Add(time.Minute))
	if err != nil || !changed || len(second) != 1 {
		t.Fatalf("second made=%d changed=%v err=%v", len(second), changed, err)
	}
	if str(second[0], "id") != cardID {
		t.Fatalf("card id changed: %s vs %s", str(second[0], "id"), cardID)
	}
	if status := str(objectOrEmpty(second[0]["proactive"]), "status"); status != "ready" {
		t.Fatalf("partial removal should keep the card ready, got %s", status)
	}
	xml := str(second[0], "xml")
	if strings.Contains(xml, "合成私密标记乙") {
		t.Fatalf("withdrawn material text survived: %s", xml)
	}
	if !strings.Contains(xml, "合成私密标记甲") {
		t.Fatalf("remaining material disappeared: %s", xml)
	}
	if !strings.Contains(xml, "已经找不到，已从简报中移除") {
		t.Fatalf("missing-material note not shown: %s", xml)
	}
}

func TestMaterialBriefStaleWhenAllMaterialsAreGone(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	state["facts"] = []any{}
	state["discoveries"] = []any{}
	state["sources"] = []any{}
	if _, changed, err := RunProactive(state, now.Add(time.Minute)); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	artifacts := objectsOrEmpty(state["artifacts"])
	if len(artifacts) != 1 {
		t.Fatalf("artifacts=%d", len(artifacts))
	}
	proactive := objectOrEmpty(artifacts[0]["proactive"])
	if status := str(proactive, "status"); status != "stale" {
		t.Fatalf("status=%s", status)
	}
	xml := str(artifacts[0], "xml")
	for _, banned := range []string{"合成私密标记甲", "合成私密标记乙", "合成推断丁", "合成来源甲里有一段可以引用的原文"} {
		if strings.Contains(xml, banned) {
			t.Fatalf("stale card kept %q: %s", banned, xml)
		}
	}
}

func TestMaterialBriefCorrectionKeepsUserVersionAndShowsChange(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	first, _, err := RunProactive(state, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("made=%d err=%v", len(first), err)
	}
	oldSummary := materialBriefSummary("合成目标甲", "把材料整理成评审前能看的对照简报",
		[]object{{"source": "合成来源甲", "status": "本人确认", "text": "合成私密标记甲", "full": "合成私密标记甲"}},
		nil, []string{"先看材料"})
	artifact := objectsOrEmpty(state["artifacts"])[0]
	artifact["plan"] = object{
		"text": "我的版本：先核对甲，再看乙。", "textMode": "user",
		"userNote": "我的版本：先核对甲，再看乙。", "systemCopy": oldSummary,
	}
	state["artifacts"] = toAnyObjects([]object{artifact})
	goal := objectsOrEmpty(state["goals"])[0]
	brief := objectOrEmpty(goal["brief"])
	brief["text"] = "我的版本：先核对甲，再看乙。"
	brief["textMode"] = "user"
	brief["userNote"] = "我的版本：先核对甲，再看乙。"
	brief["computedSummary"] = oldSummary
	goal["brief"] = brief
	state["goals"] = toAnyObjects([]object{goal})

	facts := objectsOrEmpty(state["facts"])
	facts[0]["text"] = "合成私密标记甲（已更正）"
	facts[0]["quote"] = "合成私密标记甲（已更正）"
	facts[0]["version"] = 2
	state["facts"] = toAnyObjects(facts)

	second, changed, err := RunProactive(state, now.Add(time.Minute))
	if err != nil || !changed || len(second) != 1 {
		t.Fatalf("second made=%d changed=%v err=%v", len(second), changed, err)
	}
	xml := str(second[0], "xml")
	if !strings.Contains(xml, "我的版本：先核对甲，再看乙。") {
		t.Fatalf("user version lost: %s", xml)
	}
	if !strings.Contains(xml, `<result kind="user">`) {
		t.Fatalf("result should stay the user's: %s", xml)
	}
	if !strings.Contains(xml, "合成私密标记甲（已更正）") {
		t.Fatalf("corrected material missing from the change note: %s", xml)
	}
	if strings.Contains(xml, ">合成私密标记甲<") {
		t.Fatalf("old wording revived in the card body: %s", xml)
	}
	if !strings.Contains(xml, "<change>") {
		t.Fatalf("missing change note: %s", xml)
	}
}

func TestMaterialBriefUnrelatedCorrectionDoesNotRebuild(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	facts := objectsOrEmpty(state["facts"])
	facts[2]["text"] = "没有勾选的私密标记丙（已更正）"
	facts[2]["version"] = 2
	state["facts"] = toAnyObjects(facts)
	made, changed, err := RunProactive(state, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 0 || changed {
		t.Fatalf("unrelated correction rebuilt the brief: made=%d changed=%v", len(made), changed)
	}
}

func TestMaterialBriefRepeatTickDoesNotDuplicate(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	made, changed, err := RunProactive(state, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 0 || changed {
		t.Fatalf("repeat tick made=%d changed=%v", len(made), changed)
	}
	if count := len(objectsOrEmpty(state["artifacts"])); count != 1 {
		t.Fatalf("artifacts=%d", count)
	}
}

func briefCheckState(now time.Time) object {
	long := strings.Repeat("这是一段与核对项无关的背景说明。", 40) +
		"注意：预算上限不超过 5000 元，且不包含差旅费。"
	return object{
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
}

func briefResultText(t *testing.T, xml string) string {
	t.Helper()
	match := regexp.MustCompile(`(?s)<result[^>]*>(.*?)</result>`).FindStringSubmatch(xml)
	if len(match) != 2 {
		t.Fatalf("no result in %s", xml)
	}
	return strings.ReplaceAll(match[1], "&quot;", `"`)
}

// 取代旧的 TestMaterialBriefRequestDrivesCheckItemsAndFindsLateClue：
// 请求里的核对项不再由词法引擎逐项匹配。快照的契约是：请求原文一字
// 不动地保存，材料原文（含长材料尾部的关键句）进入节选，并声明未
// 调用模型——分析留给授权后的真实模型。
func TestMaterialBriefQuotesRequestAndHonesty(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	result := briefResultText(t, str(made[0], "xml"))
	for _, want := range []string{
		"请求原文：整理成评审前能看的对照简报，重点核对：预算上限、交付日期、是否包含培训。",
		"注意：预算上限不超过 5000 元，且不包含差旅费。",
		"未调用模型，尚未分析请求、判断答案或生成建议。",
		"只是所选材料的原文节选，不是请求的完成结果",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("missing %q in result:\n%s", want, result)
		}
	}
	for _, banned := range []string{"按你的核对项", "待确认表述", "按材料逐条：", "没有匹配到相关原句"} {
		if strings.Contains(result, banned) {
			t.Fatalf("lexical check machinery survived: %q\n%s", banned, result)
		}
	}
}

// 取代旧的 TestMaterialBriefReportsUnansweredCheckItem：快照不再报告
// 「材料里没有找到」。请求里提到的词若材料从未提到，它在快照中的唯一
// 出现就是请求原文本身，没有任何关于它的判断。
func TestMaterialBriefMentionsNoClaimAboutUncoveredItem(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	goal := objectsOrEmpty(state["goals"])[0]
	brief := objectOrEmpty(goal["brief"])
	brief["request"] = "整理成评审前能看的对照简报，重点核对：预算上限、交付日期、违约责任。"
	goal["brief"] = brief
	state["goals"] = toAnyObjects([]object{goal})
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	result := briefResultText(t, xml)
	if !strings.Contains(result, "违约责任") {
		t.Fatalf("request echo lost the item:\n%s", result)
	}
	if strings.Contains(result, "没有找到") || strings.Contains(result, "未回答") || strings.Contains(result, "没有匹配到相关原句") {
		t.Fatalf("snapshot made a claim about coverage:\n%s", result)
	}
	if !strings.Contains(xml, "尚未分析你的请求") {
		t.Fatalf("question lost the honesty wording: %s", xml)
	}
}

func TestMaterialBriefLateCorrectionDropsOldSummaryAndHistory(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	first, _, err := RunProactive(state, now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first made=%d err=%v", len(first), err)
	}
	goal := objectsOrEmpty(state["goals"])[0]
	brief := objectOrEmpty(goal["brief"])
	brief["computedSummary"] = "旧摘要：预算上限 5000 元，不包含差旅费。"
	brief["summaryHistory"] = []any{object{"text": "更旧的摘要：预算上限 5000 元。"}}
	goal["brief"] = brief
	state["goals"] = toAnyObjects([]object{goal})

	sources := objectsOrEmpty(state["sources"])
	sources[0]["text"] = strings.Repeat("这是一段与核对项无关的背景说明。", 40) +
		"注意：预算上限不超过 8000 元，且不包含差旅费。"
	sources[0]["version"] = 2
	state["sources"] = toAnyObjects(sources)

	second, changed, err := RunProactive(state, now.Add(time.Minute))
	if err != nil || !changed || len(second) != 1 {
		t.Fatalf("second made=%d changed=%v err=%v", len(second), changed, err)
	}
	xml := str(second[0], "xml")
	result := briefResultText(t, xml)
	if !strings.Contains(result, "8000 元") {
		t.Fatalf("corrected number missing:\n%s", result)
	}
	if strings.Contains(xml, "旧摘要") || strings.Contains(xml, "更旧的摘要") {
		t.Fatalf("old summary survived the correction: %s", xml)
	}
	goal = objectsOrEmpty(state["goals"])[0]
	brief = objectOrEmpty(goal["brief"])
	if _, ok := brief["summaryHistory"]; ok {
		t.Fatalf("summaryHistory was kept: %v", brief["summaryHistory"])
	}
	if !strings.Contains(str(brief, "computedSummary"), "8000 元") {
		t.Fatalf("computedSummary was not replaced: %s", str(brief, "computedSummary"))
	}
}

func TestMaterialBriefLongSentenceKeepsTailQualifier(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	long := strings.Repeat("这是一段没有句号的背景说明", 40) + "，预算上限不超过 5000 元，且不包含差旅费。"
	sources := objectsOrEmpty(state["sources"])
	sources[0]["text"] = long
	state["sources"] = toAnyObjects(sources)
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	xml := str(made[0], "xml")
	materialText := regexp.MustCompile(`(?s)<item[^>]*kind="source"[^>]*>.*?<text>(.*?)</text>`).FindStringSubmatch(xml)
	if len(materialText) != 2 {
		t.Fatalf("no source text in %s", xml)
	}
	if !strings.Contains(materialText[1], "不包含差旅费") || !strings.Contains(materialText[1], "……") {
		t.Fatalf("material excerpt swallowed the tail: %s", materialText[1])
	}
	result := briefResultText(t, xml)
	if !strings.Contains(result, "不超过 5000 元") || !strings.Contains(result, "不包含差旅费") {
		t.Fatalf("check-item excerpt swallowed the qualifier or negation:\n%s", result)
	}
}

// 取代旧的 TestMaterialBriefNoCheckItemsFallsBackToMaterials：快照不再
// 区分「有核对项/无核对项」两种模式，永远按「来源（状态）+ 原文」
// 逐份列出所选材料。
func TestMaterialBriefSummaryListsMaterialsVerbatim(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefCheckState(now)
	goal := objectsOrEmpty(state["goals"])[0]
	brief := objectOrEmpty(goal["brief"])
	brief["request"] = "把材料整理成评审前能看的对照简报"
	goal["brief"] = brief
	state["goals"] = toAnyObjects([]object{goal})
	made, _, err := RunProactive(state, now)
	if err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	result := briefResultText(t, str(made[0], "xml"))
	if !strings.Contains(result, "合成来源甲（原始资料）") {
		t.Fatalf("materials not listed with source and status:\n%s", result)
	}
	if !strings.Contains(result, "不包含差旅费") {
		t.Fatalf("listing did not keep the long source's tail:\n%s", result)
	}
	if strings.Contains(result, "按材料逐条：") {
		t.Fatalf("old fallback wording survived:\n%s", result)
	}
}

func TestMaterialBriefCancelAndCompletedGoalInvalidate(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := briefState(now)
	if made, _, err := RunProactive(state, now); err != nil || len(made) != 1 {
		t.Fatalf("made=%d err=%v", len(made), err)
	}
	goal := objectsOrEmpty(state["goals"])[0]
	delete(goal, "brief")
	state["goals"] = toAnyObjects([]object{goal})
	if _, changed, err := RunProactive(state, now.Add(time.Minute)); err != nil || !changed {
		t.Fatalf("cancel changed=%v err=%v", changed, err)
	}
	artifact := objectsOrEmpty(state["artifacts"])[0]
	if status := str(objectOrEmpty(artifact["proactive"]), "status"); status != "stale" {
		t.Fatalf("cancel status=%s", status)
	}
	if strings.Contains(str(artifact, "xml"), "合成私密标记甲") {
		t.Fatalf("cancelled brief kept its material: %s", str(artifact, "xml"))
	}

	state2 := briefState(now)
	if made, _, err := RunProactive(state2, now); err != nil || len(made) != 1 {
		t.Fatalf("second made=%d err=%v", len(made), err)
	}
	goal2 := objectsOrEmpty(state2["goals"])[0]
	goal2["step"] = "已达成收尾"
	state2["goals"] = toAnyObjects([]object{goal2})
	if _, changed, err := RunProactive(state2, now.Add(time.Minute)); err != nil || !changed {
		t.Fatalf("completed changed=%v err=%v", changed, err)
	}
	artifact2 := objectsOrEmpty(state2["artifacts"])[0]
	if status := str(objectOrEmpty(artifact2["proactive"]), "status"); status != "stale" {
		t.Fatalf("completed status=%s", status)
	}
}
