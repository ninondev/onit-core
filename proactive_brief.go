package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// buildMaterialBrief preserves selected material as a labelled snapshot.
// It does not interpret requests, rank passages or infer answers without a model.
func buildMaterialBrief(state object, goal object, sources, facts, discoveries []object, now time.Time, existing map[string]object, dismissed map[string]bool) (object, bool) {
	id := str(goal, "id")
	title := strings.TrimSpace(str(goal, "title"))
	if id == "" || title == "" || str(goal, "step") == "已达成收尾" {
		return nil, false
	}
	brief := objectOrEmpty(goal["brief"])
	if len(brief) == 0 || brief["ignored"] == true {
		return nil, false
	}
	request := strings.TrimSpace(str(brief, "request"))
	refs := objectsOrEmpty(brief["materialRefs"])
	if request == "" || len(refs) == 0 {
		return nil, false
	}
	key := "brief:" + id
	if dismissed[key] {
		return nil, false
	}
	cardID := "widget:prepared-brief-" + id
	if old := existing[cardID]; old != nil && old["answered"] != nil {
		return nil, false
	}

	materials := []object{}
	verify := []object{}
	basis := []proactiveBasis{}
	missing := []string{}
	missingRefs := []string{}
	seen := map[string]bool{}
	for _, ref := range refs {
		refID := str(ref, "id")
		if refID == "" {
			continue
		}
		kind := str(ref, "kind")
		if kind == "" {
			kind = "fact"
		}
		dedupeKey := kind + ":" + refID
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true
		switch kind {
		case "fact":
			fact := findFact(facts, refID)
			if fact == nil {
				note := "有一项依据已经找不到，已从简报中移除。"
				missing = append(missing, note)
				missingRefs = append(missingRefs, dedupeKey)
				verify = append(verify, object{"kind": "missing", "ref": refID, "text": note})
				continue
			}
			version := int(number(fact["version"]))
			body := strings.TrimSpace(str(fact, "text"))
			if body == "" {
				body = strings.TrimSpace(str(fact, "quote"))
			}
			status := "未确认记录"
			if fact["confirmed"] == true {
				status = "本人确认"
			}
			sourceLabel := factSourceTitle(state, fact)
			if status == "未确认记录" {
				sourceLabel = strings.TrimSuffix(sourceLabel, "（未确认）")
			}
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "fact", "ref": refID,
				"version": version, "status": status,
				"source": sourceLabel,
				"text":   briefExcerpt(body, 240), "full": body,
			})
			basis = append(basis, proactiveBasis{Kind: "fact", ID: refID, Version: version})
			if fact["confirmed"] != true {
				verify = append(verify, object{"kind": "fact", "ref": refID,
					"text": "「" + truncate(body, 40) + "」还没有本人确认。"})
			}
			if body == "" {
				verify = append(verify, object{"kind": "fact", "ref": refID,
					"text": "「" + factSourceTitle(state, fact) + "」这条记录没有可引用的正文。"})
			}
		case "discovery":
			discovery := findDiscovery(discoveries, refID)
			if discovery == nil || str(discovery, "status") == "stale" {
				note := "有一项推断已经撤回或失效，已从简报中移除。"
				missing = append(missing, note)
				missingRefs = append(missingRefs, dedupeKey)
				verify = append(verify, object{"kind": "missing", "ref": refID, "text": note})
				continue
			}
			version := int(number(discovery["version"]))
			body := strings.TrimSpace(str(discovery, "title"))
			status := "推断"
			if str(discovery, "status") == "corrected" {
				status = "本人更正"
			}
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "discovery", "ref": refID,
				"version": version, "status": status,
				"source": discoverySourceTitle(discovery),
				"text":   briefExcerpt(body, 240), "full": body,
			})
			basis = append(basis, proactiveBasis{Kind: "discovery", ID: refID, Version: version})
			if status == "推断" {
				verify = append(verify, object{"kind": "discovery", "ref": refID,
					"text": "「" + truncate(body, 40) + "」是推断，不是原始记录。"})
			}
		case "source":
			source := findSource(sources, refID)
			if source == nil {
				note := "有一份资料已经找不到，已从简报中移除。"
				missing = append(missing, note)
				missingRefs = append(missingRefs, dedupeKey)
				verify = append(verify, object{"kind": "missing", "ref": refID, "text": note})
				continue
			}
			if str(objectOrEmpty(source["calendarChange"]), "kind") == "cancelled" {
				note := "有一份资料已经取消，已从简报中移除。"
				missing = append(missing, note)
				missingRefs = append(missingRefs, dedupeKey)
				verify = append(verify, object{"kind": "missing", "ref": refID, "text": note})
				continue
			}
			body := strings.TrimSpace(str(source, "text"))
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "source", "ref": refID,
				"version": int(number(source["version"])), "status": "原始资料",
				"source": sourceTitle(source),
				"text":   briefExcerpt(body, 240), "full": body,
			})
			basis = append(basis, proactiveBasis{Kind: "source", ID: refID})
			if body == "" {
				verify = append(verify, object{"kind": "source", "ref": refID,
					"text": "「" + sourceTitle(source) + "」没有可引用的正文。"})
			}
		default:
			note := "有一项材料的类型无法识别，已跳过。"
			missing = append(missing, note)
			missingRefs = append(missingRefs, dedupeKey)
			verify = append(verify, object{"kind": "missing", "ref": refID, "text": note})
		}
	}
	if len(materials) == 0 {
		return nil, false
	}

	methods := []string{}
	for _, method := range objectsOrEmpty(goal["approvedMethods"]) {
		if text := strings.TrimSpace(str(method, "text")); text != "" {
			methods = append(methods, text)
		}
	}

	systemSummary := materialBriefSummary(title, request, materials, verify, methods)
	userText := strings.TrimSpace(str(brief, "text"))
	// 旧存档可能把未改动的系统摘要存在 text 里。没有 textMode 时，只要它
	// 和 computedSummary 相同，就仍按系统内容处理，不能当成用户原话。
	if mode := str(brief, "textMode"); mode == "system" || (mode == "" && userText != "" && userText == strings.TrimSpace(str(brief, "computedSummary"))) {
		userText = ""
	}
	resultText, resultKind := systemSummary, "system"
	if userText != "" {
		resultText, resultKind = userText, "user"
		brief["textMode"] = "user"
	}
	changeText := ""
	if userText != "" {
		old := strings.TrimSpace(str(brief, "computedSummary"))
		summaryChanged := brief["summaryChanged"] == true
		if summaryChanged || (old != "" && old != systemSummary) {
			changeText = "当前材料快照和你改过的版本不同：" + truncate(systemSummary, 240)
		}
	}
	delete(brief, "summaryChanged")
	// 材料被撤掉或更正后，旧摘要里可能还留着它的原文。简报不保留摘要历史：
	// 旧内容不能借 summaryHistory 再次进入卡片正文或下一次重算。
	delete(brief, "summaryHistory")
	if old := strings.TrimSpace(str(brief, "computedSummary")); old != "" && old != systemSummary {
		delete(brief, "computedSummary")
	}
	brief["computedSummary"] = systemSummary
	brief["computedAt"] = now.UTC().Format(time.RFC3339)
	goal["brief"] = brief

	question := "这里只保存了所选材料快照，尚未分析你的请求。"
	if len(missing) > 0 {
		question = "有材料已不可用，快照仅保留当前可用材料；尚未分析你的请求。"
	}

	inputs := materialBriefInputsFingerprint(brief, materials, missingRefs)
	fp := materialBriefFingerprint(title, request, materials, verify, methods, userText, systemSummary, changeText, question)
	updated := false
	if old := existing[cardID]; old != nil {
		oldProactive := objectOrEmpty(old["proactive"])
		if str(oldProactive, "status") == "stale" {
			updated = true
		} else if str(oldProactive, "fingerprint") == fp {
			return nil, false
		} else {
			updated = true
		}
	}

	// 简报是长期可接着用的成果，不按天换卡；首次生成后保留原日期，
	// Today 会把仍然有效的简报跨天列出来。
	date := now.Format("2006-01-02")
	if old := existing[cardID]; old != nil {
		if stored := str(old, "date"); stored != "" {
			date = stored
		}
	}
	why := "仅展示你勾选的材料快照；没有模型参与推断，尚未理解或执行你的请求。"
	xml := materialBriefXML(cardID, title, why, request, materials, verify, resultText, resultKind, changeText, question)
	proactive := object{
		"kind": "material-brief", "key": key, "createdAt": now.UTC().Format(time.RFC3339),
		"fingerprint": fp, "inputs": inputs, "basis": toAnyBasis(basis), "status": "ready",
	}
	if updated {
		proactive["updated"] = true
	}
	card := object{
		"id": cardID, "date": date, "type": "prepared", "position": 0,
		"xml": xml, "reportID": "", "reportTitle": "", "reportSummary": "", "reportColorRamp": "",
		"reportCreatedAt": "", "reportGoalSlug": "", "goalId": id,
		"proactive": proactive,
	}
	return card, true
}

func briefSentences(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	out := []string{}
	start := 0
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		boundary := false
		switch r {
		case '。', '；', '！', '？', '!', '?', '\n', ';':
			boundary = true
		case '.':
			// ASCII 句号只在后面是空白或结尾时断句，避免切开 50.5 这类数字。
			if i+1 >= len(runes) || runes[i+1] == ' ' || runes[i+1] == '\n' || runes[i+1] == '\t' {
				prevDigit := i > 0 && runes[i-1] >= '0' && runes[i-1] <= '9'
				nextDigit := i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9'
				if !(prevDigit && nextDigit) {
					boundary = true
				}
			}
		}
		if !boundary {
			continue
		}
		part := strings.TrimSpace(string(runes[start : i+1]))
		if part != "" {
			out = append(out, part)
		}
		start = i + 1
	}
	if start < len(runes) {
		if part := strings.TrimSpace(string(runes[start:])); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func materialBriefSummary(title, request string, materials []object, verify []object, methods []string) string {
	var b strings.Builder
	b.WriteString("「" + title + "」材料快照\n")
	b.WriteString("请求原文：" + request + "\n")
	b.WriteString("未调用模型，尚未分析请求、判断答案或生成建议。以下只是所选材料的原文节选，不是请求的完成结果。\n\n")
	for i, m := range materials {
		b.WriteString(fmt.Sprintf("%d. %s（%s）\n", i+1, str(m, "source"), str(m, "status")))
		b.WriteString(briefExcerpt(str(m, "full"), 600) + "\n\n")
	}
	if len(methods) > 0 {
		b.WriteString("已保存的做法（未执行）：\n")
		for _, method := range methods {
			b.WriteString("- " + method + "\n")
		}
	}
	for _, item := range verify {
		b.WriteString("- " + str(item, "text") + "\n")
	}
	return b.String()
}

func hasBriefVerify(verify []object) bool {
	for _, item := range verify {
		switch str(item, "kind") {
		case "unanswered", "uncertain":
			continue
		}
		return true
	}
	return false
}

func briefExcerpt(text string, max int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len([]rune(text)) <= max {
		return text
	}
	sentences := briefSentences(text)
	if len(sentences) <= 1 {
		return briefHeadTail(text, max)
	}
	head := []string{}
	tail := []string{}
	total := 0
	for _, sentence := range sentences {
		if total+len([]rune(sentence)) > max/2 {
			break
		}
		head = append(head, sentence)
		total += len([]rune(sentence))
	}
	total = 0
	for i := len(sentences) - 1; i >= 0; i-- {
		sentence := sentences[i]
		if total+len([]rune(sentence)) > max/2 {
			break
		}
		tail = append([]string{sentence}, tail...)
		total += len([]rune(sentence))
	}
	if len(head) == 0 {
		head = sentences[:1]
	}
	if len(tail) == 0 {
		tail = sentences[len(sentences)-1:]
	}
	if len(head)+len(tail) >= len(sentences) {
		return text
	}
	return strings.Join(head, "") + "……" + strings.Join(tail, "")
}

func briefHeadTail(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	head := max - 80
	if head < 40 {
		head = 40
	}
	tail := 80
	if tail > len(runes)-head {
		tail = len(runes) - head
	}
	return string(runes[:head]) + "……" + string(runes[len(runes)-tail:])
}

func materialBriefInputsFingerprint(brief object, materials []object, missing []string) string {
	parts := []string{"request:" + strings.TrimSpace(str(brief, "request"))}
	for _, m := range materials {
		parts = append(parts, fmt.Sprintf("%s:%s:%d:%s", str(m, "kind"), str(m, "ref"), int(number(m["version"])), str(m, "full")))
	}
	parts = append(parts, missing...)
	return fingerprint(parts...)
}

func materialBriefFingerprint(title, request string, materials []object, verify []object, methods []string, userText, systemSummary, changeText, question string) string {
	parts := []string{"title:" + title, "request:" + request}
	for _, m := range materials {
		parts = append(parts, fmt.Sprintf("m:%s:%s:%d:%s:%s:%s", str(m, "kind"), str(m, "ref"), int(number(m["version"])), str(m, "status"), str(m, "text"), str(m, "full")))
	}
	for _, v := range verify {
		parts = append(parts, "v:"+str(v, "text"))
	}
	for _, method := range methods {
		parts = append(parts, "method:"+method)
	}
	parts = append(parts, "user:"+userText, "system:"+systemSummary, "change:"+changeText, "q:"+question)
	return fingerprint(parts...)
}

func materialBriefXML(cardID, title, why, request string, materials, verify []object, resultText, resultKind, changeText, question string) string {
	var b strings.Builder
	b.WriteString(`<prepared id="` + xmlEscape(cardID) + `" kind="material-brief" status="ready">`)
	b.WriteString(`<badge>材料快照</badge>`)
	b.WriteString(`<title>` + xmlEscape("「"+title+"」的材料快照") + `</title>`)
	b.WriteString(`<why>` + xmlEscape(why) + `</why>`)
	b.WriteString(`<request>` + xmlEscape(request) + `</request>`)
	b.WriteString(`<materials>`)
	for _, m := range materials {
		b.WriteString(`<item id="` + xmlEscape(str(m, "id")) + `" kind="` + xmlEscape(str(m, "kind")) + `" ref="` + xmlEscape(str(m, "ref")) + `" version="` + strconv.Itoa(int(number(m["version"]))) + `" status="` + xmlEscape(str(m, "status")) + `" chars="` + strconv.Itoa(len([]rune(str(m, "full")))) + `">`)
		b.WriteString(`<source>` + xmlEscape(str(m, "source")) + `</source>`)
		b.WriteString(`<text>` + xmlEscape(str(m, "text")) + `</text>`)
		b.WriteString(`</item>`)
	}
	b.WriteString(`</materials>`)
	if len(verify) > 0 {
		b.WriteString(`<verify>`)
		for _, item := range verify {
			b.WriteString(`<item kind="` + xmlEscape(str(item, "kind")) + `" ref="` + xmlEscape(str(item, "ref")) + `">` + xmlEscape(str(item, "text")) + `</item>`)
		}
		b.WriteString(`</verify>`)
	}
	b.WriteString(`<result kind="` + xmlEscape(resultKind) + `">` + xmlEscape(resultText) + `</result>`)
	if changeText != "" {
		b.WriteString(`<change>` + xmlEscape(changeText) + `</change>`)
	}
	if question != "" {
		b.WriteString(`<question>` + xmlEscape(question) + `</question>`)
	}
	b.WriteString(`</prepared>`)
	return b.String()
}
