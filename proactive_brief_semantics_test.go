package main

import (
	"strings"
	"testing"
	"time"
)

// 这些用例来自独立审计，原文测的是词法核对引擎（关键词命中、
// 「尚未确定」词典、逐项匹配清单）。那套引擎已删：快照不再判断任何
// 核对项是否已回答，只保存请求原文与材料原文，并声明未调用模型。
// 保留下来的不变量是：原文逐字进快照、截断必须可见、绝不冒充答案，
// 以及只有被勾选的材料才进入快照。真正的核对属于授权后的真实模型。

const briefHonestyLine = "未调用模型，尚未分析请求、判断答案或生成建议。"

func briefSemanticState(now time.Time, request, text string) object {
	return object{
		"version": 2,
		"sources": []any{
			object{
				"id": "semantic-s", "kind": "self", "origin": "note",
				"title": "独立合成条款", "text": text, "version": 1,
				"at": now.Format(time.RFC3339),
			},
		},
		"facts":       []any{},
		"discoveries": []any{},
		"goals": []any{
			object{
				"id": "semantic-g", "title": "核对课程条件", "step": "准备材料", "unit": "分钟",
				"at": now.Format(time.RFC3339), "approvedMethods": []any{},
				"approvedBasis": []any{}, "entries": []any{},
				"brief": object{
					"request": request,
					"materialRefs": []any{
						object{"kind": "source", "id": "semantic-s", "version": 1},
					},
				},
			},
		},
		"artifacts": []any{},
	}
}

func briefSemanticResult(t *testing.T, request, text string) string {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	state := briefSemanticState(now, request, text)
	goal := objectsOrEmpty(state["goals"])[0]
	card, ok := buildMaterialBrief(state, goal, objectsOrEmpty(state["sources"]), nil, nil, now, map[string]object{}, map[string]bool{})
	if !ok || card == nil {
		t.Fatal("no brief")
	}
	return briefResultText(t, str(card, "xml"))
}

// 材料明说「尚未确定」。快照必须原样保留这句话、绝不把它报成
// 已回答，也不再用「待确认说法」词典给这句话贴标签。
func TestMaterialBriefUnknownIsNotAnswered(t *testing.T) {
	result := briefSemanticResult(t, "核对：退款到账时间。", "退款到账时间尚未确定，必须询问课程方。")
	if !strings.Contains(result, "请求原文：核对：退款到账时间。") {
		t.Fatalf("request text missing:\n%s", result)
	}
	if !strings.Contains(result, "退款到账时间尚未确定，必须询问课程方。") {
		t.Fatalf("material sentence missing:\n%s", result)
	}
	if !strings.Contains(result, briefHonestyLine) {
		t.Fatalf("honesty line missing:\n%s", result)
	}
	for _, banned := range []string{"找到了原文", "待确认说法", "尚未确定」这样"} {
		if strings.Contains(result, banned) {
			t.Fatalf("snapshot still judges the material: %q\n%s", banned, result)
		}
	}
}

// 「尚未确定」和「整项没提到」的区分属于理解，不属于快照。新的契约：
// 两种情况都只给原文和诚实声明，不输出任何一种判断。
func TestMaterialBriefDistinguishesUnknownFromMissing(t *testing.T) {
	result := briefSemanticResult(t, "核对：退款到账时间、违约责任。", "退款到账时间尚未确定，必须询问课程方。")
	if !strings.Contains(result, "退款到账时间尚未确定，必须询问课程方。") {
		t.Fatalf("material sentence missing:\n%s", result)
	}
	if !strings.Contains(result, briefHonestyLine) {
		t.Fatalf("honesty line missing:\n%s", result)
	}
	for _, banned := range []string{"找到了原文", "待确认说法", "没有匹配到", "- 违约责任", "退款到账时间："} {
		if strings.Contains(result, banned) {
			t.Fatalf("snapshot still classifies items: %q\n%s", banned, result)
		}
	}
}

func TestMaterialBriefKeepsAdjacentFeeQualification(t *testing.T) {
	result := briefSemanticResult(t, "核对：手续费。", "手续费为50元。该费用仅在纸质教材已经寄出时收取。未寄出则不收取。")
	for _, want := range []string{
		"手续费为50元。",
		"该费用仅在纸质教材已经寄出时收取。",
		"未寄出则不收取。",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("fee excerpt lost %q:\n%s", want, result)
		}
	}
}

func TestMaterialBriefNegationPreserved(t *testing.T) {
	result := briefSemanticResult(t, "核对：手续费。", "手续费不包含税。该费用仅在纸质教材寄出后收取。")
	for _, want := range []string{"手续费不包含税。", "该费用仅在纸质教材寄出后收取。"} {
		if !strings.Contains(result, want) {
			t.Fatalf("negation or condition lost: %q\n%s", want, result)
		}
	}
}

func TestMaterialBriefEnglishMaterialBoundary(t *testing.T) {
	result := briefSemanticResult(t, "核对：refund timing。",
		"The course introduction is unrelated. The refund timing is not yet determined; ask the course provider. More unrelated background.")
	if !strings.Contains(result, "The refund timing is not yet determined;") {
		t.Fatalf("English material lost:\n%s", result)
	}
	if !strings.Contains(result, briefHonestyLine) {
		t.Fatalf("honesty line missing:\n%s", result)
	}
}

// 材料远超节选上限时，快照保留头尾并明确标记省略；被截掉的中段
// 不得以「已包含」的姿态出现，也不能假装完整。
func TestMaterialBriefLongMaterialIsMarkedExcerpt(t *testing.T) {
	text := strings.Repeat("这是不相关的课程介绍。", 70) +
		"退款期限是开课前七天。" +
		strings.Repeat("另附不相关背景说明。", 70)
	result := briefSemanticResult(t, "核对：退款期限。", text)
	if !strings.Contains(result, "……") {
		t.Fatalf("truncation marker missing:\n%s", result[:400])
	}
	if !strings.Contains(result, "这是不相关的课程介绍。") {
		t.Fatalf("head excerpt missing:\n%s", result[:400])
	}
	if !strings.Contains(result, "另附不相关背景说明。") {
		t.Fatalf("tail excerpt missing:\n%s", result[len(result)-400:])
	}
	if strings.Contains(result, "退款期限是开课前七天。") {
		t.Fatalf("omitted middle sentence appeared without a completeness claim being checked:\n%s", result)
	}
}

// 快照只收被勾选的材料；没勾选的另一份资料一个字都不能进来。
// 「邻居句子算不算相关」这类取舍不再由规则决定。
func TestMaterialBriefUnrelatedNeighborNotPulledIn(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	state := briefSemanticState(now, "核对：手续费。", "手续费为50元。这是一段背景说明。")
	sources := objectsOrEmpty(state["sources"])
	sources = append(sources, object{
		"id": "semantic-other", "kind": "self", "origin": "note",
		"title": "另一份资料", "text": "另附不相关背景说明。", "version": 1, "at": now.Format(time.RFC3339),
	})
	state["sources"] = toAnyObjects(sources)
	goal := objectsOrEmpty(state["goals"])[0]
	card, ok := buildMaterialBrief(state, goal, sources, nil, nil, now, map[string]object{}, map[string]bool{})
	if !ok {
		t.Fatal("no brief")
	}
	xml := str(card, "xml")
	if !strings.Contains(xml, "手续费为50元。") {
		t.Fatalf("selected material missing: %s", xml)
	}
	if strings.Contains(xml, "另附不相关背景说明") {
		t.Fatalf("unselected source leaked into the brief: %s", xml)
	}
}

// 没有可识别的「待确认短语」时，旧的词法词典会输出「这次没有识别到
// 明确的待确认说法」。现在统一换成一句诚实声明：未调用模型。
func TestMaterialBriefNoRecognizedPhraseIsNotCompleteness(t *testing.T) {
	result := briefSemanticResult(t, "核对：最终费用。", "最终费用必须等检测后确认。")
	if !strings.Contains(result, "最终费用必须等检测后确认。") {
		t.Fatalf("material sentence missing:\n%s", result)
	}
	if !strings.Contains(result, briefHonestyLine) {
		t.Fatalf("honesty line missing:\n%s", result)
	}
	for _, banned := range []string{"识别到", "待确认", "没有列出", "无，"} {
		if strings.Contains(result, banned) {
			t.Fatalf("lexical fallback wording survived: %q\n%s", banned, result)
		}
	}
}

// 词面匹配连不上「送修材料」和「送修时需提供…」；旧引擎把这个缺陷
// 打成清单。现在快照根本不做匹配，原句原样保留，交给模型理解。
func TestMaterialBriefDoesNotPretendPhraseMatching(t *testing.T) {
	result := briefSemanticResult(t, "核对：送修材料。", "送修时需提供购机发票和保修卡。")
	if !strings.Contains(result, "送修时需提供购机发票和保修卡。") {
		t.Fatalf("material sentence missing:\n%s", result)
	}
	if !strings.Contains(result, briefHonestyLine) {
		t.Fatalf("honesty line missing:\n%s", result)
	}
	if strings.Contains(result, "送修材料：") || strings.Contains(result, "没有匹配到") {
		t.Fatalf("phrase matching survived:\n%s", result)
	}
}
