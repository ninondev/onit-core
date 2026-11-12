package main

// Persistent goal review records live inside each goal's brief in the dossier
// JSON, so they are stored in the same SQLite row as the goal. The JS side runs
// the three steps; this file normalizes and validates every dossier write, and
// invalidates a review when a selected fact or source is changed or withdrawn
// by the service. A review never claims real-world completion.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	grMaxSelected = 40
	grMaxResult   = 48 * 1024
	grMaxHistory  = 3
	grMaxRequest  = 4000
)

var grStatuses = map[string]bool{
	"idle": true, "running": true, "saving": true, "completed": true,
	"failed": true, "cancelled": true, "stale": true,
}

var grStepStatuses = map[string]bool{
	"pending": true, "running": true, "done": true, "failed": true, "cancelled": true,
}

var grRefKinds = map[string]bool{"fact": true, "discovery": true, "source": true}

// grHash matches the JS FNV-1a over UTF-16 code units.
func grHash(value string) string {
	var h uint32 = 0x811c9dc5
	for _, unit := range utf16.Encode([]rune(value)) {
		h ^= uint32(unit)
		h *= 0x01000193
	}
	return fmt.Sprintf("%08x", h)
}

func grRefs(goal object) []object {
	brief, _ := goal["brief"].(object)
	if brief == nil {
		return nil
	}
	items, err := objects(brief["materialRefs"])
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]object, 0, len(items))
	for _, item := range items {
		kind := str(item, "kind")
		if !grRefKinds[kind] {
			kind = "fact"
		}
		id := str(item, "id")
		if id == "" || seen[kind+":"+id] {
			continue
		}
		seen[kind+":"+id] = true
		out = append(out, object{"kind": kind, "id": id})
		if len(out) >= grMaxSelected {
			break
		}
	}
	return out
}

func grSelectionHash(goal object) string {
	refs := grRefs(goal)
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, str(ref, "kind")+":"+str(ref, "id"))
	}
	sort.Strings(keys)
	return grHash(strings.Join(keys, "|"))
}

// grMaterial returns the current identity and a readable excerpt of one
// selected material. The excerpt is what the review body shows, so a result
// cannot consist of labels and hashes alone. A missing, stale, calendar or
// conversation source is never replaced by a lookalike.
func grMaterial(d *dossierRows, ref object) (object, bool) {
	kind, id := str(ref, "kind"), str(ref, "id")
	switch kind {
	case "fact":
		for _, fact := range d.rows["facts"] {
			if str(fact, "id") != id {
				continue
			}
			text := str(fact, "text")
			if strings.TrimSpace(text) == "" {
				return nil, false
			}
			version, err := integer(fact["version"])
			if err != nil || version < 1 {
				version = 1
			}
			item := object{
				"kind": "fact", "id": id, "label": grClip(text, 120),
				"version": version, "textHash": grHash(text), "at": str(fact, "at"),
				"preview": grClip(text, 240), "text": text,
			}
			grAttachFactOrigin(item, fact)
			return item, true
		}
	case "discovery":
		for _, row := range d.rows["discoveries"] {
			if str(row, "id") != id || str(row, "status") == "stale" {
				continue
			}
			title, body := str(row, "title"), str(row, "body")
			text := strings.TrimSpace(title + "\n" + body)
			if text == "" {
				return nil, false
			}
			version, err := integer(row["version"])
			if err != nil || version < 1 {
				version = 1
			}
			return object{
				"kind": "discovery", "id": id, "label": grClip(title, 120),
				"version": version, "textHash": grHash(title + body), "at": str(row, "at"),
				"preview": grClip(text, 240), "text": text,
			}, true
		}
	case "source":
		source, ok := d.sources[id]
		if !ok || str(source, "origin") == "conversation" {
			return nil, false
		}
		text := str(source, "text")
		if strings.TrimSpace(text) == "" || source["calendar"] == true {
			return nil, false
		}
		if change, _ := source["calendarChange"].(object); change != nil && str(change, "kind") == "cancelled" {
			return nil, false
		}
		return object{
			"kind": "source", "id": id, "label": grClip(str(source, "title"), 120),
			"version": 1, "textHash": grHash(text), "at": str(source, "at"),
			"preview": grClip(text, 240), "text": text,
		}, true
	}
	return nil, false
}

func grAttachFactOrigin(dst, fact object) {
	if sid := str(fact, "sourceId"); sid != "" {
		dst["sourceId"] = sid
	}
	if q := str(fact, "quote"); q != "" {
		dst["quote"] = q
	}
	if fact["confirmed"] == true {
		dst["confirmed"] = true
	}
	if attr := str(fact, "attribution"); attr != "" {
		dst["attribution"] = attr
	}
	ev, err := objects(fact["evidence"])
	if err != nil || len(ev) == 0 {
		return
	}
	slim := make([]any, 0, len(ev))
	for _, row := range ev {
		item := object{}
		if sid := str(row, "sourceId"); sid != "" {
			item["sourceId"] = sid
		}
		if q := str(row, "quote"); q != "" {
			item["quote"] = q
		}
		if len(item) > 0 {
			slim = append(slim, item)
		}
	}
	if len(slim) > 0 {
		dst["evidence"] = slim
	}
}

func grMaterialsCurrent(d *dossierRows, review object) bool {
	snapshot, err := objects(review["materialSnapshot"])
	if err != nil {
		return false
	}
	selected, err := objects(review["selected"])
	if err != nil || len(selected) > grMaxSelected {
		return false
	}
	if len(snapshot) == 0 {
		for _, ref := range selected {
			if _, ok := grMaterial(d, ref); !ok {
				return false
			}
		}
		return true
	}
	if len(snapshot) != len(selected) {
		return false
	}
	for _, item := range snapshot {
		current, ok := grMaterial(d, item)
		if !ok {
			return false
		}
		version, err := integer(item["version"])
		if err != nil || version != mustInt(current["version"]) {
			return false
		}
		if str(item, "textHash") != str(current, "textHash") {
			return false
		}
	}
	return true
}

func mustInt(value any) int {
	n, _ := integer(value)
	return n
}

func grProgressCurrent(goal object, review object) bool {
	entries, err := objects(goal["entries"])
	if err != nil {
		return false
	}
	byID := map[string]object{}
	for _, entry := range entries {
		byID[str(entry, "id")] = entry
	}
	ids, err := grStrings(review["progressEntryIds"])
	if err != nil || len(ids) > len(entries) {
		return false
	}
	for _, id := range ids {
		if _, ok := byID[id]; !ok {
			return false
		}
	}
	snapshot, _ := review["progressSnapshot"].(object)
	if snapshot == nil {
		return true
	}
	items, err := objects(snapshot["entries"])
	if err != nil {
		return false
	}
	for _, item := range items {
		entry, ok := byID[str(item, "id")]
		if !ok {
			return false
		}
		if number(entry["amount"]) != number(item["amount"]) {
			return false
		}
		if str(entry, "note") != str(item, "note") {
			return false
		}
	}
	return true
}

func grStrings(v any) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, ErrInvalid
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch value := item.(type) {
		case string:
			if value == "" {
				return nil, ErrInvalid
			}
			out = append(out, value)
		case object:
			id := str(value, "id")
			if id == "" {
				return nil, ErrInvalid
			}
			out = append(out, id)
		default:
			return nil, ErrInvalid
		}
	}
	return out, nil
}

func grAny(items []object) []any {
	out := make([]any, len(items))
	for i := range items {
		out[i] = items[i]
	}
	return out
}

// grResultProgressCurrent checks the saved result against the goal's current
// entries. A result that does not match the records is rejected, not staled:
// staling is for a basis that changed after a legitimate run.
func grResultProgressCurrent(goal object, result object) bool {
	progress, _ := result["progress"].(object)
	if progress == nil {
		return false
	}
	items, err := objects(progress["entries"])
	if err != nil {
		return false
	}
	entries, err := objects(goal["entries"])
	if err != nil {
		return false
	}
	byID := map[string]object{}
	for _, entry := range entries {
		byID[str(entry, "id")] = entry
	}
	for _, item := range items {
		row, ok := byID[str(item, "id")]
		if !ok || number(row["amount"]) != number(item["amount"]) || str(row, "note") != str(item, "note") {
			return false
		}
	}
	return true
}

func grResultMaterialsCurrent(d *dossierRows, result object) bool {
	items, err := objects(result["materials"])
	if err != nil {
		return false
	}
	for _, item := range items {
		current, ok := grMaterial(d, item)
		if !ok || mustInt(item["version"]) != mustInt(current["version"]) ||
			str(item, "textHash") != str(current, "textHash") {
			return false
		}
	}
	return true
}

func grSelectedMatchesBrief(goal object, review object) bool {
	want := grRefs(goal)
	got, err := objects(review["selected"])
	if err != nil || len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, ref := range got {
		set[str(ref, "kind")+":"+str(ref, "id")] = true
	}
	for _, ref := range want {
		if !set[str(ref, "kind")+":"+str(ref, "id")] {
			return false
		}
	}
	return true
}

func grClearResult(review object, reason string) {
	review["status"] = "stale"
	review["staleReason"] = reason
	delete(review, "result")
	review["resultId"] = ""
	steps, _ := objects(review["steps"])
	for _, step := range steps {
		if str(step, "status") == "running" {
			step["status"] = "cancelled"
			step["error"] = reason
		}
	}
}

// normalizeGoalReviews runs before validation on every dossier write. It never
// revives a stale review and never repairs a result into a completed one.
func normalizeGoalReviews(d *dossierRows) {
	archiveID := str(d.fields, "archiveId")
	for _, goal := range d.rows["goals"] {
		brief, _ := goal["brief"].(object)
		if brief == nil {
			continue
		}
		if history, err := objects(brief["reviewHistory"]); err == nil {
			if len(history) > grMaxHistory {
				brief["reviewHistory"] = grAny(history[:grMaxHistory])
			}
		} else {
			delete(brief, "reviewHistory")
		}
		review, _ := brief["review"].(object)
		if review == nil {
			continue
		}
		if str(review, "owner") != archiveID || str(review, "goalId") != str(goal, "id") {
			grClearResult(review, "这次复盘不属于当前目标或档案，已停止。")
			continue
		}
		if str(goal, "step") == "已达成收尾" {
			status := str(review, "status")
			if status == "idle" || status == "running" || status == "saving" || status == "completed" {
				grClearResult(review, "目标已经收尾，这次复盘已过期。")
			}
			continue
		}
		if refs, err := objects(review["selected"]); err == nil {
			if len(refs) > grMaxSelected {
				review["selected"] = grAny(refs[:grMaxSelected])
			}
		} else {
			delete(review, "selected")
		}
		status := str(review, "status")
		if status == "failed" || status == "cancelled" || status == "stale" {
			delete(review, "result")
			review["resultId"] = ""
		}
		// A changed selection hash means the goal's brief changed, which is a
		// scope withdrawal, not a malformed write. Clear the derived result
		// and keep the withdrawal itself saveable. If the hash still matches
		// the brief but the review's own list does not, validation rejects it
		// as a forged list instead of silently accepting it.
		if status == "idle" || status == "running" || status == "saving" || status == "completed" {
			if str(review, "selectionHash") != grSelectionHash(goal) {
				grClearResult(review, "选中的材料范围已经变化，这次复盘已过期。")
				continue
			}
			if !grSelectedMatchesBrief(goal, review) {
				// The hash still matches the brief, so only the review's own
				// list was changed. Leave it for validation to reject rather
				// than repairing a malformed write into a stale review.
				continue
			}
		}
		if status == "completed" {
			if !grMaterialsCurrent(d, review) || !grProgressCurrent(goal, review) {
				grClearResult(review, "选中的材料或进展已经变化，这次复盘已过期。")
				continue
			}
			if !grResultArtifactsForged(review) && !grResultArtifactsCurrent(d, goal, review) {
				grClearResult(review, "引用的准备产物已经不可读取，这次复盘已过期。")
				continue
			}
		}
		if result, ok := review["result"].(object); ok && result != nil {
			body := str(result, "body")
			if utf8.RuneCountInString(body) > grMaxResult {
				runes := []rune(body)
				result["body"] = string(runes[:grMaxResult]) + "\n…（内容过长，已截断）"
			}
		}
	}
}

func validateGoalReviews(d *dossierRows) error {
	for _, goal := range d.rows["goals"] {
		brief, _ := goal["brief"].(object)
		if brief == nil {
			continue
		}
		review, ok := brief["review"].(object)
		if !ok || review == nil {
			continue
		}
		if err := grValidateReview(d, goal, review); err != nil {
			return err
		}
	}
	return nil
}

func grValidateReview(d *dossierRows, goal object, review object) error {
	if str(review, "id") == "" || str(review, "owner") == "" {
		return ErrInvalid
	}
	if str(review, "goalId") != str(goal, "id") {
		return ErrInvalid
	}
	request := str(review, "request")
	if request == "" || utf8.RuneCountInString(request) > grMaxRequest {
		return ErrInvalid
	}
	status := str(review, "status")
	if !grStatuses[status] {
		return ErrInvalid
	}
	selected, err := objects(review["selected"])
	if err != nil || len(selected) > grMaxSelected {
		return ErrInvalid
	}
	for _, ref := range selected {
		if !grRefKinds[str(ref, "kind")] || str(ref, "id") == "" {
			return ErrInvalid
		}
	}
	if (status == "idle" || status == "running" || status == "saving" || status == "completed") &&
		(str(review, "selectionHash") == "" || str(review, "selectionHash") != grSelectionHash(goal) ||
			!grSelectedMatchesBrief(goal, review)) {
		return ErrInvalid
	}
	steps, err := objects(review["steps"])
	if err != nil || len(steps) != 3 {
		return ErrInvalid
	}
	for i, want := range []string{"progress", "check", "save"} {
		if str(steps[i], "id") != want || !grStepStatuses[str(steps[i], "status")] {
			return ErrInvalid
		}
	}
	if attempts, err := integer(review["attempts"]); err != nil || attempts < 0 {
		return ErrInvalid
	}
	result, hasResult := review["result"].(object)
	if status == "completed" {
		if !hasResult || result == nil {
			return ErrInvalid
		}
		if str(result, "id") == "" || str(result, "id") != str(review, "resultId") {
			return ErrInvalid
		}
		if str(result, "goalId") != str(goal, "id") || str(result, "reviewId") != str(review, "id") {
			return ErrInvalid
		}
		if strings.TrimSpace(str(result, "body")) == "" {
			return ErrInvalid
		}
		if utf8.RuneCountInString(str(result, "body")) > grMaxResult+64 {
			return ErrInvalid
		}
		for _, step := range steps {
			if str(step, "status") != "done" {
				return ErrInvalid
			}
		}
		if !grMaterialsCurrent(d, review) || !grProgressCurrent(goal, review) {
			return ErrInvalid
		}
		if grResultArtifactsForged(review) || !grResultArtifactsCurrent(d, goal, review) {
			return ErrInvalid
		}
		if !grResultMaterialsCurrent(d, result) || !grResultProgressCurrent(goal, result) {
			return ErrInvalid
		}
		artifacts, err := objects(result["artifacts"])
		if err != nil {
			return ErrInvalid
		}
		readable := map[string]bool{}
		for _, artifact := range grReadableArtifacts(d, goal) {
			readable[str(artifact, "id")] = true
		}
		for _, artifact := range artifacts {
			if !readable[str(artifact, "id")] {
				return ErrInvalid
			}
		}
		refs, err := objects(result["refs"])
		if err != nil || len(refs) != len(selected) {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			seen[str(ref, "kind")+":"+str(ref, "id")] = true
		}
		for _, ref := range selected {
			if !seen[str(ref, "kind")+":"+str(ref, "id")] {
				return ErrInvalid
			}
		}
		progress, ok := result["progress"].(object)
		if !ok || progress == nil {
			return ErrInvalid
		}
		items, err := objects(progress["entries"])
		if err != nil {
			return ErrInvalid
		}
		byID := map[string]object{}
		goalEntries, err := objects(goal["entries"])
		if err != nil {
			return ErrInvalid
		}
		for _, row := range goalEntries {
			byID[str(row, "id")] = row
		}
		for _, item := range items {
			row, ok := byID[str(item, "id")]
			if !ok || number(row["amount"]) != number(item["amount"]) {
				return ErrInvalid
			}
		}
		if _, err := grStrings(result["missing"]); err != nil {
			return ErrInvalid
		}
		materials, err := objects(result["materials"])
		if err != nil || len(materials) != len(selected) {
			return ErrInvalid
		}
		for _, material := range materials {
			if strings.TrimSpace(str(material, "preview")) == "" {
				return ErrInvalid
			}
		}
		return nil
	}
	if status == "saving" {
		if hasResult && result != nil {
			if str(result, "id") == "" || str(result, "id") != str(review, "resultId") {
				return ErrInvalid
			}
		}
		return nil
	}
	if hasResult || str(review, "resultId") != "" {
		return ErrInvalid
	}
	return nil
}

// androidInvalidateGoalReviews is the service-side half of the same rule: a
// fact or source changed outside the local editor invalidates any review that
// selected it, in the same transaction, and clears the readable result.
func androidInvalidateGoalReviews(tx *sql.Tx, user string, factIDs, sourceIDs map[string]bool) error {
	rows, err := tx.Query("SELECT id,extra FROM android_fields WHERE user_id=? AND kind='goals'", user)
	if err != nil {
		return err
	}
	type goalRow struct{ id, extra string }
	var goals []goalRow
	for rows.Next() {
		var row goalRow
		if err := rows.Scan(&row.id, &row.extra); err != nil {
			rows.Close()
			return err
		}
		goals = append(goals, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, row := range goals {
		fields, err := parseObject([]byte(row.extra))
		if err != nil {
			return err
		}
		brief, _ := fields["brief"].(object)
		if brief == nil {
			continue
		}
		review, _ := brief["review"].(object)
		if review == nil {
			continue
		}
		status := str(review, "status")
		if status != "idle" && status != "running" && status != "saving" && status != "completed" {
			continue
		}
		changed := str(review, "selectionHash") != grSelectionHash(fields)
		if !changed {
			refs, err := objects(review["selected"])
			if err != nil {
				return err
			}
			for _, ref := range refs {
				kind, id := str(ref, "kind"), str(ref, "id")
				if (kind == "fact" && factIDs[id]) || (kind == "source" && sourceIDs[id]) {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		grClearResult(review, "选中的资料已更正或撤回，这次复盘已过期。")
		if _, err := tx.Exec("UPDATE android_fields SET extra=? WHERE user_id=? AND kind='goals' AND id=?",
			encode(fields), user, row.id); err != nil {
			return err
		}
	}
	return nil
}
