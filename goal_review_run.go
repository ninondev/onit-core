package main

// The goal review is executed here, not in the WebView. The three restricted
// local steps read the goal's current records and the explicitly selected
// materials from SQLite, check their current text and version, and save a
// readable result back into the same dossier row. Every step is a separate
// committed transition, so a later process can continue only the unfinished
// steps. No model, network request or external tool is used.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	grActionStart   = "start"
	grActionAdvance = "advance"
	grActionRun     = "run"
	grActionRestart = "restart"
	grActionCancel  = "cancel"
	grActionShow    = "show"

	grDefaultRequest = "保存我选的材料和已记录进展，不分析内容或判断完成。"
	grMaxAttempts    = 3
)

// These snapshots preserve user-selected records without natural-language inference.
func grNormClause(text string) string {
	return strings.TrimSpace(strings.Trim(text, " \t。.!！？?;；：:，,、"))
}

func grBodyFooter(review object) string {
	return "未调用模型，尚未分析请求、识别缺项或判断现实完成情况。这只是已保存记录和所选材料的快照。"
}

func grStripMaterialText(materials []object) []object {
	for _, material := range materials {
		delete(material, "text")
	}
	return materials
}

func grNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func grID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "gr_" + hex.EncodeToString(b)
}

func grRound(v float64) float64 { return math.Round(v*1000) / 1000 }

func grAmountString(v float64) string {
	return strconv.FormatFloat(grRound(v), 'f', -1, 64)
}

func grSlice(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func grClip(text string, limit int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return grSlice(text, limit) + "…"
}

func grClone(v any) any {
	switch x := v.(type) {
	case object:
		out := make(object, len(x))
		for k, val := range x {
			out[k] = grClone(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = grClone(val)
		}
		return out
	default:
		return v
	}
}

func grAnyStrings(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func grProgressOf(goal object) object {
	entries, _ := objects(goal["entries"])
	sort.SliceStable(entries, func(i, j int) bool {
		return str(entries[i], "at") > str(entries[j], "at")
	})
	total := 0.0
	out := make([]object, 0, len(entries))
	for _, entry := range entries {
		amount := number(entry["amount"])
		total += amount
		out = append(out, object{
			"id": str(entry, "id"), "amount": grRound(amount),
			"note": str(entry, "note"), "at": str(entry, "at"),
		})
	}
	return object{"total": grRound(total), "entries": grAny(out)}
}

func grProgressKey(v any) string {
	progress, _ := v.(object)
	if progress == nil {
		return ""
	}
	entries, _ := objects(progress["entries"])
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, str(entry, "id")+":"+grAmountString(number(entry["amount"]))+":"+str(entry, "note"))
	}
	return strings.Join(parts, ",")
}

// grReadableArtifacts reads from the split dossier, not from the raw state:
// splitDossier moves reports out of fields into rows, so reading state["reports"]
// here would make a legitimate weekly report invisible to validation and stale
// a completed result that cites it.
func grReadableArtifacts(d *dossierRows, goal object) []object {
	out := []object{}
	artifacts, _ := objects(d.fields["artifacts"])
	for _, artifact := range artifacts {
		if str(artifact, "goalId") != str(goal, "id") || str(artifact, "type") != "prepared" {
			continue
		}
		// An artifact without an id cannot be opened by the existing card view.
		if str(artifact, "id") == "" {
			continue
		}
		// A title alone is not a readable artifact. The prepared card must
		// carry its XML body, which is what the existing card view opens.
		if strings.TrimSpace(str(artifact, "xml")) == "" {
			continue
		}
		proactive, _ := artifact["proactive"].(object)
		if proactive != nil && str(proactive, "status") == "stale" {
			continue
		}
		if artifact["dismissed"] == true {
			continue
		}
		kind, title := "prepared", "准备产物"
		if proactive != nil {
			if value := str(proactive, "kind"); value != "" {
				kind = value
			}
			if value := str(proactive, "title"); value != "" {
				title = value
			} else if value := str(artifact, "title"); value != "" {
				title = value
			}
		} else if value := str(artifact, "title"); value != "" {
			title = value
		}
		at := str(artifact, "createdAt")
		if at == "" {
			at = str(artifact, "at")
		}
		out = append(out, object{"id": str(artifact, "id"), "kind": kind, "title": title, "at": at})
	}
	if brief, _ := goal["brief"].(object); brief != nil {
		if preparation, _ := brief["modelPreparation"].(object); preparation != nil {
			if current, _ := preparation["current"].(object); current != nil && str(current, "status") == "complete" {
				readable := false
				title := "模型准备"
				if result, _ := current["result"].(object); result != nil {
					if sections, _ := objects(result["sections"]); len(sections) > 0 {
						readable = true
					}
					if value := str(result, "title"); value != "" {
						title = value
					}
				}
				if edit, _ := current["userEdit"].(object); edit != nil && strings.TrimSpace(str(edit, "text")) != "" {
					readable = true
				}
				if readable {
					id := str(current, "attemptId")
					if id == "" {
						id = str(current, "id")
					}
					if id == "" {
						id = "current"
					}
					at := str(current, "finishedAt")
					if at == "" {
						at = str(current, "at")
					}
					out = append(out, object{"id": "model:" + id, "kind": "model-preparation", "title": title, "at": at})
				}
			}
		}
	}
	for _, report := range d.rows["reports"] {
		id := str(report, "id")
		if id == "" {
			continue
		}
		rows, _ := objects(report["rows"])
		for _, row := range rows {
			if str(row, "goalId") == str(goal, "id") && number(row["count"]) > 0 {
				out = append(out, object{
					"id": id, "kind": "report",
					"title": "周报 " + grSlice(str(report, "at"), 10), "at": str(report, "at"),
				})
				break
			}
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// grResultArtifactsForged reports a result artifact that was never in the
// step's own artifact snapshot. A deleted artifact is a legitimate later state
// change; an id that never existed in the snapshot is a malformed result.
func grResultArtifactsForged(review object) bool {
	partial, _ := review["partial"].(object)
	if partial == nil {
		return false
	}
	allowed := map[string]bool{}
	for _, artifact := range objectsOrEmpty(partial["artifacts"]) {
		allowed[str(artifact, "id")] = true
	}
	result, _ := review["result"].(object)
	if result == nil {
		return false
	}
	for _, artifact := range objectsOrEmpty(result["artifacts"]) {
		if !allowed[str(artifact, "id")] {
			return true
		}
	}
	return false
}

// grResultArtifactsCurrent requires every artifact the result cites to still
// be readable now. A deleted or unreadable artifact invalidates the result
// instead of leaving a stale "readable artifact" claim.
func grResultArtifactsCurrent(d *dossierRows, goal object, review object) bool {
	result, _ := review["result"].(object)
	if result == nil {
		return true
	}
	artifacts, err := objects(result["artifacts"])
	if err != nil {
		return false
	}
	readable := map[string]bool{}
	for _, artifact := range grReadableArtifacts(d, goal) {
		readable[str(artifact, "id")] = true
	}
	for _, artifact := range artifacts {
		if !readable[str(artifact, "id")] {
			return false
		}
	}
	return true
}

func grMissingOf(d *dossierRows, goal object, selected []object, progress object, artifacts []object) []string {
	missing := []string{}
	entries, _ := objects(progress["entries"])
	if len(entries) == 0 {
		missing = append(missing, "这个目标还没有记录任何进展；现在只能整理目标本身，不能写成做过什么。")
	}
	noNote := 0
	for _, entry := range entries {
		if str(entry, "note") == "" {
			noNote++
		}
	}
	if noNote > 0 {
		missing = append(missing, fmt.Sprintf("有 %d 条进展没有说明，复盘只能按数量列，不能补写原因。", noNote))
	}
	for _, ref := range selected {
		if _, ok := grMaterial(d, ref); !ok {
			missing = append(missing, "选中的材料「"+str(ref, "kind")+":"+str(ref, "id")+"」已经不可用或版本不明。")
		}
	}
	if len(artifacts) == 0 {
		missing = append(missing, "还没有可打开的准备产物；这次只能整理进展记录。")
	}
	target := number(goal["target"])
	if target > 0 && number(progress["total"]) < target {
		unit := ""
		if value := str(goal, "unit"); value != "" {
			unit = " " + value
		}
		missing = append(missing, "离目标还差 "+grAmountString(target-number(progress["total"]))+unit+"；这是记录里的数字，不代表现实进度。")
	}
	if len(selected) == 0 {
		missing = append(missing, "没有选中材料；这次只整理目标里的进展记录。")
	}
	return missing
}

func grBodyOf(goal object, review object, result object) string {
	unit := ""
	if value := str(goal, "unit"); value != "" {
		unit = " " + value
	}
	lines := []string{}
	lines = append(lines, "记录快照："+str(goal, "title"))
	lines = append(lines, "请求原文："+str(review, "request"))
	lines = append(lines, grBodyFooter(review))
	if note := str(review, "changeNote"); note != "" {
		lines = append(lines, "这次与上一版的变化："+note)
	}
	lines = append(lines, "")
	progress, _ := result["progress"].(object)
	entries, _ := objects(progress["entries"])
	lines = append(lines, "已记录的进展（"+strconv.Itoa(len(entries))+" 条，共 "+grAmountString(number(progress["total"]))+unit+"）")
	if len(entries) == 0 {
		lines = append(lines, "- 没有记录。")
	}
	for _, entry := range entries {
		note := str(entry, "note")
		if note == "" {
			note = "（没有说明）"
		} else {
			note = "，" + note
		}
		lines = append(lines, "- "+grSlice(str(entry, "at"), 10)+"："+grAmountString(number(entry["amount"]))+unit+note)
	}
	lines = append(lines, "")
	materials, _ := objects(result["materials"])
	lines = append(lines, "这次选中的材料（"+strconv.Itoa(len(materials))+" 份）")
	if len(materials) == 0 {
		lines = append(lines, "- 没有选中材料。")
	}
	for _, material := range materials {
		lines = append(lines, "- "+str(material, "label")+"（"+str(material, "kind")+":"+str(material, "id")+"，版本 "+strconv.Itoa(mustInt(material["version"]))+"）")
		preview := str(material, "preview")
		if preview == "" {
			preview = "（没有可读正文）"
		}
		lines = append(lines, "  正文："+preview)
		quote := str(material, "quote")
		if quote != "" && grNormClause(quote) != grNormClause(preview) {
			lines = append(lines, "  原文引文："+grClip(quote, 200))
		}
		if note := str(material, "currentnessNote"); note != "" {
			lines = append(lines, "  "+note)
		}
	}
	lines = append(lines, "")
	artifacts, _ := objects(result["artifacts"])
	lines = append(lines, "已有可读取产物（"+strconv.Itoa(len(artifacts))+" 份）")
	if len(artifacts) == 0 {
		lines = append(lines, "- 没有可打开的准备产物。")
	}
	for _, artifact := range artifacts {
		lines = append(lines, "- "+str(artifact, "title")+"（"+str(artifact, "kind")+"，"+str(artifact, "id")+"）")
	}
	lines = append(lines, "")
	lines = append(lines, "记录结构状态（不是语义核对）")
	missing, _ := grStrings(result["missing"])
	if len(missing) == 0 {
		lines = append(lines, "- 已选记录可读取；没有判断材料是否完整或问题是否得到回答。")
	}
	for _, item := range missing {
		lines = append(lines, "- "+item)
	}
	lines = append(lines, "")
	lines = append(lines, "下一步待确认")
	lines = append(lines, "- 以上只整理已记录内容和选中材料；是否在现实中完成，需要你自己确认。")
	lines = append(lines, "- 如果要补依据，请先在目标里补记录或重新选材料，再重新运行这次复盘。")
	lines = append(lines, "")
	lines = append(lines, grBodyFooter(review))
	return strings.Join(lines, "\n")
}

func grBuildResult(goal object, review object) object {
	partial, _ := review["partial"].(object)
	progress, _ := grClone(partial["progress"]).(object)
	artifacts := []any{}
	if value, ok := grClone(partial["artifacts"]).([]any); ok {
		artifacts = value
	}
	materials := []any{}
	if value, ok := grClone(partial["materials"]).([]any); ok {
		materials = value
	}
	missing := []any{}
	if value, ok := grClone(partial["missing"]).([]any); ok {
		missing = value
	}
	entries, _ := objects(progress["entries"])
	entryParts := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryParts = append(entryParts, str(entry, "id")+":"+grAmountString(number(entry["amount"])))
	}
	artifactRows, _ := objects(artifacts)
	artifactParts := make([]string, 0, len(artifactRows))
	for _, artifact := range artifactRows {
		artifactParts = append(artifactParts, str(artifact, "id"))
	}
	id := "gr_" + grHash(strings.Join([]string{
		str(review, "id"), str(review, "selectionHash"),
		strings.Join(entryParts, ","), strings.Join(artifactParts, ","),
	}, "|"))
	result := object{
		"id": id, "reviewId": str(review, "id"), "goalId": str(goal, "id"),
		"createdAt": grNow(), "request": str(review, "request"),
		"progress": progress, "artifacts": artifacts, "materials": materials,
		"missing": missing, "refs": grClone(review["selected"]), "body": "",
	}
	result["body"] = grBodyOf(goal, review, result)
	body := str(result, "body")
	if utf8.RuneCountInString(body) > grMaxResult {
		result["body"] = grSlice(body, grMaxResult) + "\n…（内容过长，已截断）"
	}
	return result
}

func grNewReview(state object, goal object, request string, selected []object) object {
	owner := str(state, "archiveId")
	if owner == "" {
		owner = opaqueID(18)
		state["archiveId"] = owner
	}
	return object{
		"id": grID(), "owner": owner, "goalId": str(goal, "id"), "request": request,
		"status": "idle", "selected": grAny(selected), "selectionHash": grSelectionHash(goal),
		"progressEntryIds": []any{}, "progressSnapshot": nil, "materialSnapshot": nil,
		"steps": []any{
			object{"id": "progress", "label": "读取当前有效进展和获选材料", "status": "pending"},
			object{"id": "check", "label": "核对记录版本与可用性", "status": "pending"},
			object{"id": "save", "label": "保存记录快照与引用", "status": "pending"},
		},
		"attempts": 0, "error": "", "partial": nil, "result": nil, "resultId": "",
		"createdAt": grNow(), "updatedAt": grNow(),
	}
}

func grArchiveReview(brief object, review object) {
	history, _ := objects(brief["reviewHistory"])
	history = append([]object{review}, history...)
	if len(history) > grMaxHistory {
		history = history[:grMaxHistory]
	}
	brief["reviewHistory"] = grAny(history)
}

func grOpenReview(state object, goal object, request string) (object, bool) {
	brief, _ := goal["brief"].(object)
	if brief == nil {
		brief = object{}
		goal["brief"] = brief
	}
	existing, _ := brief["review"].(object)
	owner := str(state, "archiveId")
	if existing != nil {
		if str(existing, "owner") != owner || str(existing, "goalId") != str(goal, "id") {
			grClearResult(existing, "这次复盘不属于当前目标或档案，已停止。")
		}
		reusable := map[string]bool{
			"idle": true, "running": true, "saving": true, "completed": true,
			"failed": true, "cancelled": true, "stale": true,
		}
		if str(existing, "owner") == owner && str(existing, "goalId") == str(goal, "id") &&
			str(existing, "request") == request && str(existing, "selectionHash") == grSelectionHash(goal) &&
			reusable[str(existing, "status")] {
			return existing, false
		}
		grArchiveReview(brief, existing)
	}
	review := grNewReview(state, goal, request, grRefs(goal))
	brief["review"] = review
	return review, true
}

func grRestartReview(state object, goal object, request string) object {
	brief, _ := goal["brief"].(object)
	if brief == nil {
		brief = object{}
		goal["brief"] = brief
	}
	if existing, _ := brief["review"].(object); existing != nil {
		grArchiveReview(brief, existing)
	}
	review := grNewReview(state, goal, request, grRefs(goal))
	brief["review"] = review
	return review
}

func grCancelReview(state object, goal object) object {
	review := grReviewOf(goal)
	if review == nil {
		return nil
	}
	status := str(review, "status")
	if status != "idle" && status != "running" && status != "saving" {
		return review
	}
	review["status"] = "cancelled"
	review["cancelledAt"] = grNow()
	review["result"] = nil
	review["resultId"] = ""
	review["updatedAt"] = grNow()
	steps, _ := objects(review["steps"])
	for _, step := range steps {
		if str(step, "status") == "running" {
			step["status"] = "cancelled"
			step["error"] = "用户取消了这次复盘。"
		}
	}
	return review
}

func grStepProgress(d *dossierRows, state object, goal object, review object) string {
	if status := str(review, "status"); status == "cancelled" || status == "stale" {
		return "cancelled"
	}
	if str(review, "owner") != str(state, "archiveId") || str(review, "goalId") != str(goal, "id") {
		grClearResult(review, "这次复盘不属于当前目标或档案，已停止。")
		return "stale"
	}
	if str(review, "selectionHash") != grSelectionHash(goal) {
		grClearResult(review, "选中的材料范围已经变化，这次复盘已过期。")
		return "stale"
	}
	// Freeze the selected material versions at the first step. A material
	// corrected between the first and second step must stale the task instead
	// of being silently re-read as if it had always been current.
	selected, _ := objects(review["selected"])
	materials := make([]object, 0, len(selected))
	for _, ref := range selected {
		material, ok := grMaterial(d, ref)
		if !ok {
			grClearResult(review, "选中的材料已经撤回或版本不明，这次复盘已过期。")
			return "stale"
		}
		materials = append(materials, material)
	}
	materials = grStripMaterialText(materials)
	progress := grProgressOf(goal)
	entries, _ := objects(progress["entries"])
	ids := make([]any, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, str(entry, "id"))
	}
	review["progressEntryIds"] = ids
	review["progressSnapshot"] = progress
	review["materialSnapshot"] = grAny(materials)
	review["partial"] = object{
		"progress": progress, "artifacts": grAny(grReadableArtifacts(d, goal)),
		"materials": grAny(materials), "missing": []any{},
	}
	return "done"
}

func grStepCheck(d *dossierRows, state object, goal object, review object) string {
	if status := str(review, "status"); status == "cancelled" || status == "stale" {
		return "cancelled"
	}
	if str(review, "selectionHash") != grSelectionHash(goal) {
		grClearResult(review, "选中的材料范围已经变化，这次复盘已过期。")
		return "stale"
	}
	selected, _ := objects(review["selected"])
	materials := make([]object, 0, len(selected))
	for _, ref := range selected {
		material, ok := grMaterial(d, ref)
		if !ok {
			grClearResult(review, "选中的材料已经撤回或版本不明，这次复盘已过期。")
			return "stale"
		}
		materials = append(materials, material)
	}
	if review["materialSnapshot"] != nil && !grMaterialsCurrent(d, review) {
		grClearResult(review, "选中的材料已经更正或版本变化，这次复盘已过期。")
		return "stale"
	}
	progress := grProgressOf(goal)
	if grProgressKey(review["progressSnapshot"]) != grProgressKey(progress) {
		grClearResult(review, "目标的进展记录已经变化，这次复盘已过期。")
		return "stale"
	}
	artifacts := grReadableArtifacts(d, goal)
	missing := grMissingOf(d, goal, selected, progress, artifacts)
	materials = grStripMaterialText(materials)
	review["materialSnapshot"] = grAny(materials)
	review["partial"] = object{
		"progress": progress, "artifacts": grAny(artifacts), "materials": grAny(materials),
		"missing": grAnyStrings(missing),
	}
	return "done"
}

func grStepByID(review object, id string) object {
	steps, _ := objects(review["steps"])
	for _, step := range steps {
		if str(step, "id") == id {
			return step
		}
	}
	return nil
}

func grPrepare(state object, goal object, review object) (string, bool) {
	status := str(review, "status")
	if status == "completed" || status == "cancelled" || status == "stale" {
		return "", true
	}
	if status == "failed" && mustInt(review["attempts"]) >= grMaxAttempts {
		return "", true
	}
	if status == "saving" {
		return "save", false
	}
	if status == "running" {
		steps, _ := objects(review["steps"])
		for _, step := range steps {
			if str(step, "status") == "running" {
				step["status"] = "pending"
				step["resumedAt"] = grNow()
			}
		}
	}
	steps, _ := objects(review["steps"])
	for _, step := range steps {
		if str(step, "status") == "done" {
			continue
		}
		step["status"] = "running"
		step["startedAt"] = grNow()
		step["error"] = ""
		review["status"] = "running"
		review["error"] = ""
		review["attempts"] = mustInt(review["attempts"]) + 1
		review["updatedAt"] = grNow()
		return str(step, "id"), false
	}
	if str(review, "resultId") != "" && review["result"] != nil {
		review["status"] = "completed"
		review["completedAt"] = grNow()
	} else {
		review["status"] = "failed"
		if str(review, "error") == "" {
			review["error"] = "这次复盘没有完成。"
		}
	}
	review["updatedAt"] = grNow()
	return "", true
}

func grExecute(d *dossierRows, state object, goal object, review object, stepID string) (string, string) {
	switch stepID {
	case "progress":
		return grStepProgress(d, state, goal, review), ""
	case "check":
		return grStepCheck(d, state, goal, review), ""
	case "save":
		if str(review, "status") == "saving" {
			if result, _ := review["result"].(object); result != nil &&
				str(result, "id") == str(review, "resultId") && strings.TrimSpace(str(result, "body")) != "" {
				return "complete", ""
			}
			review["status"] = "running"
			if step := grStepByID(review, "save"); step != nil {
				step["status"] = "pending"
				step["resumedAt"] = grNow()
			}
			return "retry", ""
		}
		if review["partial"] == nil {
			return "failed", "还没有可保存的复盘内容。"
		}
		if !grMaterialsCurrent(d, review) {
			grClearResult(review, "选中的材料已经更正或版本变化，这次复盘已过期。")
			return "stale", ""
		}
		result := grBuildResult(goal, review)
		review["result"] = result
		review["resultId"] = str(result, "id")
		review["status"] = "saving"
		review["updatedAt"] = grNow()
		return "saving", ""
	}
	return "failed", "未知步骤。"
}

func grCompleteSave(state object, goal object, review object) bool {
	if str(review, "status") != "saving" {
		return false
	}
	result, _ := review["result"].(object)
	if result == nil || str(result, "id") == "" || str(result, "id") != str(review, "resultId") ||
		strings.TrimSpace(str(result, "body")) == "" {
		return false
	}
	if step := grStepByID(review, "save"); step != nil {
		step["status"] = "done"
		step["finishedAt"] = grNow()
	}
	review["status"] = "completed"
	review["completedAt"] = grNow()
	review["updatedAt"] = grNow()
	return true
}

func grFindGoal(state object, goalID string) object {
	goals, _ := objects(state["goals"])
	for _, goal := range goals {
		if str(goal, "id") == goalID {
			return goal
		}
	}
	return nil
}

func grReviewOf(goal object) object {
	brief, _ := goal["brief"].(object)
	if brief == nil {
		return nil
	}
	review, _ := brief["review"].(object)
	return review
}

func grOwnerForRead(tx *sql.Tx, requested string) (string, error) {
	var user string
	err := tx.QueryRow("SELECT user_id FROM android_dossier WHERE slot=1").Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if requested != "" && requested != user {
		return "", ErrNotFound
	}
	return user, nil
}

func grOwnerForWrite(tx *sql.Tx, requested string) (string, error) {
	if requested == "" {
		return androidOwner(tx)
	}
	return grOwnerForRead(tx, requested)
}

func grLoadState(tx *sql.Tx, user string) (int, object, error) {
	var revision int
	var metadata sql.NullString
	if err := tx.QueryRow("SELECT revision,metadata FROM android_dossier WHERE slot=1").Scan(&revision, &metadata); err != nil {
		return 0, nil, missing(err)
	}
	state, err := loadDossier(tx, user, metadata)
	return revision, state, err
}

// grCommit writes the modified state and advances the dossier revision with a
// compare-and-swap. The caller's transaction is rolled back on conflict, so a
// stale step result cannot overwrite a newer state.
func (s *Store) grCommit(tx *sql.Tx, user string, prior object, state object, expected int) (int, object, error) {
	next, err := splitDossier(state)
	if err != nil {
		return 0, nil, err
	}
	if err = saveDossier(tx, user, prior, next); err != nil {
		return 0, nil, err
	}
	reloaded, err := loadDossier(tx, user, sql.NullString{String: encode(next.fields), Valid: true})
	if err != nil {
		return 0, nil, err
	}
	if reloaded == nil {
		reloaded = object{"version": 2}
	}
	sig := sha256.Sum256([]byte(encode(reloaded)))
	result, err := tx.Exec("UPDATE android_dossier SET revision=revision+1,signature=?,last_input=NULL WHERE slot=1 AND revision=?",
		sig[:], expected)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			return 0, nil, ErrConflict
		}
		return 0, nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, nil, err
	}
	if affected != 1 {
		return 0, nil, ErrConflict
	}
	return expected + 1, reloaded, nil
}

func (s *Store) goalReviewCreate(user, goalID, request string, restart bool) (object, error) {
	var review object
	err := s.write(func(tx *sql.Tx) error {
		owner, err := grOwnerForWrite(tx, user)
		if err != nil {
			return err
		}
		revision, state, err := grLoadState(tx, owner)
		if err != nil {
			return err
		}
		if state == nil {
			state = object{"version": 2}
		}
		goal := grFindGoal(state, goalID)
		if goal == nil {
			return ErrNotFound
		}
		if str(goal, "step") == "已达成收尾" {
			return ErrInvalid
		}
		prior := grClone(state).(object)
		if restart {
			review = grRestartReview(state, goal, request)
		} else {
			review, _ = grOpenReview(state, goal, request)
		}
		_, _, err = s.grCommit(tx, owner, prior, state, revision)
		return err
	})
	return review, err
}

func (s *Store) goalReviewShow(user, goalID string) (object, error) {
	var review object
	err := s.write(func(tx *sql.Tx) error {
		owner, err := grOwnerForRead(tx, user)
		if err != nil {
			return err
		}
		_, state, err := grLoadState(tx, owner)
		if err != nil {
			return err
		}
		if state == nil {
			return ErrNotFound
		}
		goal := grFindGoal(state, goalID)
		if goal == nil {
			return ErrNotFound
		}
		review = grReviewOf(goal)
		if review == nil {
			return ErrNotFound
		}
		return nil
	})
	return review, err
}

func (s *Store) goalReviewCancel(user, goalID string) (object, error) {
	var review object
	err := s.write(func(tx *sql.Tx) error {
		owner, err := grOwnerForWrite(tx, user)
		if err != nil {
			return err
		}
		revision, state, err := grLoadState(tx, owner)
		if err != nil {
			return err
		}
		if state == nil {
			return ErrNotFound
		}
		goal := grFindGoal(state, goalID)
		if goal == nil {
			return ErrNotFound
		}
		review = grReviewOf(goal)
		if review == nil {
			return ErrNotFound
		}
		prior := grClone(state).(object)
		review = grCancelReview(state, goal)
		_, _, err = s.grCommit(tx, owner, prior, state, revision)
		return err
	})
	return review, err
}

// goalReviewAdvance commits one transition. It returns done=true when the
// review reached a terminal state or there is no pending step.
func (s *Store) goalReviewAdvance(user, goalID string) (object, bool, error) {
	var stepID string
	var done bool
	var review object
	err := s.write(func(tx *sql.Tx) error {
		owner, err := grOwnerForWrite(tx, user)
		if err != nil {
			return err
		}
		revision, state, err := grLoadState(tx, owner)
		if err != nil {
			return err
		}
		if state == nil {
			return ErrNotFound
		}
		goal := grFindGoal(state, goalID)
		if goal == nil {
			return ErrNotFound
		}
		review = grReviewOf(goal)
		if review == nil {
			return ErrNotFound
		}
		prior := grClone(state).(object)
		if str(goal, "step") == "已达成收尾" {
			grClearResult(review, "目标已经收尾，这次复盘已过期。")
			_, _, err = s.grCommit(tx, owner, prior, state, revision)
			done = true
			return err
		}
		stepID, done = grPrepare(state, goal, review)
		_, _, err = s.grCommit(tx, owner, prior, state, revision)
		return err
	})
	if err != nil || done || stepID == "" {
		return review, true, err
	}

	outcome := ""
	reason := ""
	err = s.write(func(tx *sql.Tx) error {
		owner, err := grOwnerForWrite(tx, user)
		if err != nil {
			return err
		}
		revision, state, err := grLoadState(tx, owner)
		if err != nil {
			return err
		}
		if state == nil {
			return ErrNotFound
		}
		goal := grFindGoal(state, goalID)
		if goal == nil {
			return ErrNotFound
		}
		review = grReviewOf(goal)
		if review == nil {
			return ErrNotFound
		}
		status := str(review, "status")
		if status == "completed" || status == "cancelled" || status == "stale" {
			done = true
			return nil
		}
		step := grStepByID(review, stepID)
		if step == nil {
			return ErrInvalid
		}
		if str(step, "status") != "running" && !(stepID == "save" && str(review, "status") == "saving") {
			done = false
			return nil
		}
		prior := grClone(state).(object)
		d, err := splitDossier(state)
		if err != nil {
			return err
		}
		outcome, reason = grExecute(d, state, goal, review, stepID)
		switch outcome {
		case "done":
			step["status"] = "done"
			step["finishedAt"] = grNow()
			review["status"] = "running"
		case "complete":
			grCompleteSave(state, goal, review)
		case "saving":
			// The save step stays running until the result is read back.
		case "stale":
			step["status"] = "cancelled"
			step["finishedAt"] = grNow()
			step["error"] = reason
		case "failed":
			step["status"] = "failed"
			step["finishedAt"] = grNow()
			step["error"] = reason
			review["status"] = "failed"
			review["error"] = reason
		case "cancelled":
			step["status"] = "cancelled"
			step["finishedAt"] = grNow()
		case "retry":
			step["status"] = "pending"
			step["resumedAt"] = grNow()
			review["status"] = "running"
		}
		review["updatedAt"] = grNow()
		_, _, err = s.grCommit(tx, owner, prior, state, revision)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if outcome == "saving" {
		if err = s.write(func(tx *sql.Tx) error {
			owner, err := grOwnerForWrite(tx, user)
			if err != nil {
				return err
			}
			revision, state, err := grLoadState(tx, owner)
			if err != nil {
				return err
			}
			if state == nil {
				return ErrNotFound
			}
			goal := grFindGoal(state, goalID)
			if goal == nil {
				return ErrNotFound
			}
			review = grReviewOf(goal)
			if review == nil {
				return ErrNotFound
			}
			if str(review, "status") == "saving" {
				prior := grClone(state).(object)
				if grCompleteSave(state, goal, review) {
					_, _, err = s.grCommit(tx, owner, prior, state, revision)
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, false, err
		}
	}
	if outcome == "saving" || outcome == "complete" {
		if err = s.write(func(tx *sql.Tx) error {
			owner, err := grOwnerForRead(tx, user)
			if err != nil {
				return err
			}
			_, state, err := grLoadState(tx, owner)
			if err != nil {
				return err
			}
			if state == nil {
				return ErrNotFound
			}
			goal := grFindGoal(state, goalID)
			if goal == nil {
				return ErrNotFound
			}
			review = grReviewOf(goal)
			if review == nil {
				return ErrNotFound
			}
			return nil
		}); err != nil {
			return nil, false, err
		}
		return review, true, nil
	}
	if outcome == "done" || outcome == "retry" || outcome == "" {
		return review, false, nil
	}
	return review, true, nil
}

// GoalReview is the single backend entry point used by the CLI and the Android
// stdio bridge. user is empty for the Android slot-1 owner.
func (s *Store) GoalReview(user, goalID, action, request string) (object, error) {
	if goalID == "" {
		return nil, ErrInvalid
	}
	if request == "" {
		request = grDefaultRequest
	}
	if utf8.RuneCountInString(request) > grMaxRequest {
		return nil, ErrInvalid
	}
	switch action {
	case grActionShow:
		return s.goalReviewShow(user, goalID)
	case grActionCancel:
		return s.goalReviewCancel(user, goalID)
	case grActionStart, grActionAdvance, grActionRun, grActionRestart:
		if action == grActionStart || action == grActionRun || action == grActionRestart {
			if _, err := s.goalReviewCreate(user, goalID, request, action == grActionRestart); err != nil {
				return nil, err
			}
		}
		if action == grActionAdvance {
			if _, err := s.goalReviewShow(user, goalID); err != nil {
				return nil, err
			}
		}
		if action == grActionStart || action == grActionAdvance {
			review, _, err := s.goalReviewAdvance(user, goalID)
			return review, err
		}
		for i := 0; i < 6; i++ {
			review, done, err := s.goalReviewAdvance(user, goalID)
			if err != nil {
				return nil, err
			}
			if done {
				return review, nil
			}
		}
		return s.goalReviewShow(user, goalID)
	default:
		return nil, ErrInvalid
	}
}
