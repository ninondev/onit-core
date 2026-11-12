package main

// 时间准备：从用户明确授权的日程和材料里算出冲突、空档和可修改的结果。
// 不联网、不调用模型、不改日历、不发消息；只写回一份普通 prepared 卡。
// 目标上的 preparation 是可选配置，不新增核心对象，也不要求所有目标都有。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type scheduleBusy struct {
	ID          string
	Title       string
	Begin       int64
	End         int64
	BeforeBegin int64
	BeforeEnd   int64
}

type scheduleSlot struct {
	ID    string
	Begin int64
	End   int64
}

func stringList(v any) []string {
	rows, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if s, ok := row.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func sourceTitle(source object) string {
	title := strings.TrimSpace(str(source, "title"))
	if title == "" {
		title = strings.TrimSpace(str(source, "origin"))
	}
	if title == "" {
		title = "未命名日程"
	}
	return title
}

func slotText(begin, end int64, loc *time.Location) string {
	b, e := time.UnixMilli(begin).In(loc), time.UnixMilli(end).In(loc)
	if b.Format("2006-01-02") == e.Format("2006-01-02") {
		return b.Format("15:04") + "–" + e.Format("15:04")
	}
	return b.Format("1月2日 15:04") + "–" + e.Format("1月2日 15:04")
}

func rangeText(begin, end int64, loc *time.Location) string {
	b, e := time.UnixMilli(begin).In(loc), time.UnixMilli(end).In(loc)
	if b.Format("2006-01-02") == e.Format("2006-01-02") {
		return b.Format("1月2日 15:04") + "–" + e.Format("15:04")
	}
	return b.Format("1月2日 15:04") + "–" + e.Format("1月2日 15:04")
}

func dedupeStrings(rows []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, row := range rows {
		row = strings.TrimSpace(row)
		if row == "" || seen[row] {
			continue
		}
		seen[row] = true
		out = append(out, row)
	}
	return out
}

// preparationInputsFingerprint hashes the actual inputs a schedule-plan card
// was computed from. A source can change without a formal version bump, so the
// stale pass compares this fingerprint instead of trusting a version field.
func preparationInputsFingerprint(goal object, sources, facts, discoveries []object) string {
	prep := objectOrEmpty(goal["preparation"])
	window := objectOrEmpty(prep["window"])
	parts := []string{
		str(goal, "id"), str(goal, "title"), str(goal, "step"),
		fmt.Sprintf("window:%d:%d:%d", int64(number(window["begin"])), int64(number(window["end"])), int(number(prep["needMinutes"]))),
		fmt.Sprintf("relationPending:%t", prep["relationPending"] == true),
		fmt.Sprintf("synthetic:%t", prep["synthetic"] == true),
	}
	for _, question := range stringList(prep["questions"]) {
		parts = append(parts, "question:"+question)
	}
	for _, method := range objectsOrEmpty(goal["approvedMethods"]) {
		parts = append(parts, "method:"+strings.TrimSpace(str(method, "text")))
	}
	for _, cid := range stringList(prep["calendarIds"]) {
		source := findSource(sources, cid)
		if source == nil {
			parts = append(parts, "calendar:"+cid+":missing")
			continue
		}
		change := objectOrEmpty(source["calendarChange"])
		cal := objectOrEmpty(source["calendar"])
		if after := objectOrEmpty(change["after"]); len(after) > 0 {
			cal = after
		}
		before := objectOrEmpty(change["before"])
		parts = append(parts, fmt.Sprintf("calendar:%s:%d:%d:%d:%d:%d:%s:%s:%s",
			cid, int64(number(cal["begin"])), int64(number(cal["end"])),
			int64(number(before["begin"])), int64(number(before["end"])),
			int(number(source["version"])), str(change, "kind"),
			sourceTitle(source), str(source, "text")))
	}
	materialRefs := objectsOrEmpty(prep["materialRefs"])
	if _, ok := prep["materialRefs"]; !ok {
		materialRefs = objectsOrEmpty(goal["approvedBasis"])
	}
	for _, ref := range materialRefs {
		id := str(ref, "id")
		if id == "" {
			continue
		}
		kind := str(ref, "kind")
		if kind == "" {
			kind = "fact"
		}
		switch kind {
		case "fact":
			fact := findFact(facts, id)
			if fact == nil {
				parts = append(parts, "fact:"+id+":missing")
				continue
			}
			parts = append(parts, fmt.Sprintf("fact:%s:%d:%s:%s", id,
				int(number(fact["version"])), str(fact, "text"), str(fact, "quote")))
		case "discovery":
			discovery := findDiscovery(discoveries, id)
			if discovery == nil || str(discovery, "status") == "stale" {
				parts = append(parts, "discovery:"+id+":missing")
				continue
			}
			parts = append(parts, fmt.Sprintf("discovery:%s:%d:%s", id,
				int(number(discovery["version"])), str(discovery, "title")))
		case "source":
			source := findSource(sources, id)
			if source == nil {
				parts = append(parts, "source:"+id+":missing")
				continue
			}
			parts = append(parts, fmt.Sprintf("source:%s:%d:%s:%s", id,
				int(number(source["version"])), sourceTitle(source), str(source, "text")))
		default:
			parts = append(parts, "material:"+kind+":"+id+":unknown")
		}
	}
	return fingerprint(parts...)
}

func schedulePlanFingerprint(goal object, busy []scheduleBusy, materials []object, methods, questions []string, slots []scheduleSlot, chosenBegin, chosenEnd int64, userText, systemSummary string, synthetic bool, status string) string {
	parts := []string{str(goal, "id"), str(goal, "title"), str(goal, "step"), status, fmt.Sprint(synthetic)}
	for _, item := range busy {
		parts = append(parts, item.ID, fmt.Sprint(item.Begin), fmt.Sprint(item.End), fmt.Sprint(item.BeforeBegin), fmt.Sprint(item.BeforeEnd))
	}
	for _, m := range materials {
		parts = append(parts, str(m, "kind"), str(m, "ref"), fmt.Sprint(int(number(m["version"]))), str(m, "status"), str(m, "text"))
	}
	parts = append(parts, methods...)
	parts = append(parts, questions...)
	for _, slot := range slots {
		parts = append(parts, fmt.Sprint(slot.Begin), fmt.Sprint(slot.End))
	}
	parts = append(parts, fmt.Sprint(chosenBegin), fmt.Sprint(chosenEnd), userText, systemSummary)
	return fingerprint(parts...)
}

// buildSchedulePlan 只在目标明确带有 preparation 配置时工作。
// 缺少准备窗口、时长或日程关系时返回一张 pending 卡，只问一个问题，不编造空档。
func buildSchedulePlan(state object, goal object, sources, facts, discoveries []object, now time.Time, existing map[string]object, dismissed map[string]bool) (object, bool) {
	id := str(goal, "id")
	title := strings.TrimSpace(str(goal, "title"))
	if id == "" || title == "" || str(goal, "step") == "已达成收尾" {
		return nil, false
	}
	prep := objectOrEmpty(goal["preparation"])
	if len(prep) == 0 || prep["ignored"] == true {
		return nil, false
	}
	key := "plan:" + id
	if dismissed[key] {
		return nil, false
	}
	cardID := "widget:" + now.Format("2006-01-02") + ":prepared-plan-" + id
	if old := existing[cardID]; old != nil && old["answered"] != nil {
		return nil, false
	}

	loc := now.Location()
	window := objectOrEmpty(prep["window"])
	windowBegin, windowEnd := int64(number(window["begin"])), int64(number(window["end"]))
	need := int(number(prep["needMinutes"]))
	calendarIDs := stringList(prep["calendarIds"])
	synthetic := prep["synthetic"] == true
	relationPending := prep["relationPending"] == true
	questions := stringList(prep["questions"])

	var busy []scheduleBusy
	var basis []proactiveBasis
	var provenance []object
	var missing []string
	var calendarMissing []string
	cancelled := false
	for _, cid := range calendarIDs {
		source := findSource(sources, cid)
		if source == nil {
			calendarMissing = append(calendarMissing, "有一个日程已经找不到")
			continue
		}
		name := sourceTitle(source)
		change := objectOrEmpty(source["calendarChange"])
		if str(change, "kind") == "cancelled" {
			cancelled = true
			continue
		}
		cal := objectOrEmpty(source["calendar"])
		if after := objectOrEmpty(change["after"]); len(after) > 0 {
			cal = after
		}
		begin, end := int64(number(cal["begin"])), int64(number(cal["end"]))
		if begin <= 0 || end <= 0 || end <= begin {
			calendarMissing = append(calendarMissing, "日程「"+name+"」没有可用时间")
			continue
		}
		item := scheduleBusy{ID: cid, Title: name, Begin: begin, End: end}
		if before := objectOrEmpty(change["before"]); len(before) > 0 {
			item.BeforeBegin = int64(number(before["begin"]))
			item.BeforeEnd = int64(number(before["end"]))
		}
		busy = append(busy, item)
		basis = append(basis, proactiveBasis{Kind: "source", ID: cid})
		provenance = append(provenance, object{
			"kind": "source", "id": cid, "title": name,
			"version": int(number(source["version"])),
			"begin":   begin, "end": end,
			"beforeBegin": item.BeforeBegin, "beforeEnd": item.BeforeEnd,
		})
	}
	if cancelled {
		// 取消的日程禁用这份准备；markStalePrepared 会清掉旧卡里的时间与引文。
		return nil, false
	}

	materialRefs := objectsOrEmpty(prep["materialRefs"])
	if _, ok := prep["materialRefs"]; !ok {
		materialRefs = objectsOrEmpty(goal["approvedBasis"])
	}
	materials := []object{}
	for _, ref := range materialRefs {
		refID := str(ref, "id")
		if refID == "" {
			continue
		}
		kind := str(ref, "kind")
		if kind == "" {
			kind = "fact"
		}
		switch kind {
		case "fact":
			fact := findFact(facts, refID)
			if fact == nil {
				missing = append(missing, "有一项依据已经找不到")
				continue
			}
			version := int(number(fact["version"]))
			current := strings.TrimSpace(str(fact, "text"))
			if current == "" {
				current = strings.TrimSpace(str(fact, "quote"))
			}
			status := "未确认记录"
			if fact["confirmed"] == true {
				status = "本人确认"
			}
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "fact", "ref": refID,
				"version": version, "status": status,
				"source": factSourceTitle(state, fact), "text": truncate(current, 120),
			})
			basis = append(basis, proactiveBasis{Kind: "fact", ID: refID, Version: version})
		case "discovery":
			discovery := findDiscovery(discoveries, refID)
			if discovery == nil || str(discovery, "status") == "stale" {
				missing = append(missing, "有一项推断已经撤回或失效")
				continue
			}
			version := int(number(discovery["version"]))
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "discovery", "ref": refID,
				"version": version, "status": discoverySourceTitle(discovery),
				"source": discoverySourceTitle(discovery), "text": truncate(strings.TrimSpace(str(discovery, "title")), 120),
			})
			basis = append(basis, proactiveBasis{Kind: "discovery", ID: refID, Version: version})
		case "source":
			source := findSource(sources, refID)
			if source == nil {
				missing = append(missing, "有一项资料已经找不到")
				continue
			}
			if str(objectOrEmpty(source["calendarChange"]), "kind") == "cancelled" {
				missing = append(missing, "资料「"+sourceTitle(source)+"」已经取消")
				continue
			}
			materials = append(materials, object{
				"id": "m" + strconv.Itoa(len(materials)), "kind": "source", "ref": refID,
				"version": int(number(source["version"])), "status": "原始资料",
				"source": sourceTitle(source), "text": truncate(strings.TrimSpace(str(source, "text")), 120),
			})
			basis = append(basis, proactiveBasis{Kind: "source", ID: refID})
		default:
			missing = append(missing, "有一项依据的类型无法识别")
		}
	}

	methods := []string{}
	for _, method := range objectsOrEmpty(goal["approvedMethods"]) {
		if text := strings.TrimSpace(str(method, "text")); text != "" {
			methods = append(methods, text)
		}
	}

	// 关键输入缺失：只问一个问题，不算空档。
	critical := []string{}
	if relationPending {
		critical = append(critical, "这次准备和「"+title+"」是什么关系？")
	}
	if len(calendarIDs) == 0 {
		critical = append(critical, "这次准备要参考哪几个日程？")
	}
	if windowBegin <= 0 || windowEnd <= 0 || windowEnd <= windowBegin {
		critical = append(critical, "这次准备安排在什么时间范围内？")
	}
	if need <= 0 {
		critical = append(critical, "这次准备需要多长时间？")
	}
	// 截止日期和显式窗口谁优先还没有产品决定。两者矛盾时先问清楚，
	// 不替用户选一个，也不把窗口之外的空档当成可用。
	if dueDate := strings.TrimSpace(str(goal, "dueDate")); dueDate != "" && windowBegin > 0 {
		if due, err := time.ParseInLocation("2006-01-02", dueDate, loc); err == nil {
			if due.AddDate(0, 0, 1).UnixMilli() <= windowBegin {
				critical = append(critical, "这次准备的窗口在截止日期之后，要以哪个为准？")
			}
		}
	}
	for _, item := range calendarMissing {
		critical = append(critical, item+"，要换一个吗？")
	}
	if len(critical) == 0 && len(busy) == 0 && len(calendarIDs) > 0 {
		critical = append(critical, "这几个日程都读不到时间，要重新选吗？")
	}

	// 已经过去的时段不能推荐。窗口整体过去时如实说明并询问新窗口，
	// 不自行续到第二天，也不编造安排。
	expired := windowEnd > 0 && windowEnd <= now.UnixMilli()
	effectiveBegin := windowBegin
	if !expired && now.UnixMilli() > effectiveBegin {
		effectiveBegin = now.UnixMilli()
	}

	var slots []scheduleSlot
	var conflicts []string
	var changes []string
	if len(critical) == 0 && !expired {
		clipped := make([]scheduleBusy, 0, len(busy))
		for _, item := range busy {
			begin, end := item.Begin, item.End
			if begin < effectiveBegin {
				begin = effectiveBegin
			}
			if end > windowEnd {
				end = windowEnd
			}
			if end <= begin {
				continue
			}
			item.Begin, item.End = begin, end
			clipped = append(clipped, item)
		}
		sort.SliceStable(clipped, func(i, j int) bool { return clipped[i].Begin < clipped[j].Begin })

		for i := 0; i < len(busy); i++ {
			for j := i + 1; j < len(busy); j++ {
				if busy[i].ID == busy[j].ID {
					continue
				}
				begin := busy[i].Begin
				if busy[j].Begin > begin {
					begin = busy[j].Begin
				}
				end := busy[i].End
				if busy[j].End < end {
					end = busy[j].End
				}
				if end > begin {
					conflicts = append(conflicts, fmt.Sprintf("「%s」和「%s」重叠 %d 分钟（%s）。",
						busy[i].Title, busy[j].Title, int((end-begin)/60000), slotText(begin, end, loc)))
				}
			}
		}
		for _, item := range busy {
			if item.BeforeBegin <= 0 || item.BeforeEnd <= 0 {
				continue
			}
			if item.BeforeBegin == item.Begin && item.BeforeEnd == item.End {
				continue
			}
			delta := int((item.Begin - item.BeforeBegin) / 60000)
			direction := "时间有变化"
			if delta < 0 {
				direction = fmt.Sprintf("比原来提前了 %d 分钟", -delta)
			} else if delta > 0 {
				direction = fmt.Sprintf("比原来推迟了 %d 分钟", delta)
			}
			changes = append(changes, fmt.Sprintf("「%s」%s（原 %s，现在 %s）。",
				item.Title, direction, slotText(item.BeforeBegin, item.BeforeEnd, loc), slotText(item.Begin, item.End, loc)))
		}

		merged := make([]scheduleBusy, 0, len(clipped))
		for _, item := range clipped {
			if len(merged) == 0 || item.Begin > merged[len(merged)-1].End {
				merged = append(merged, item)
				continue
			}
			if item.End > merged[len(merged)-1].End {
				merged[len(merged)-1].End = item.End
			}
		}
		cursor := effectiveBegin
		free := [][2]int64{}
		for _, item := range merged {
			if item.Begin > cursor {
				free = append(free, [2]int64{cursor, item.Begin})
			}
			if item.End > cursor {
				cursor = item.End
			}
		}
		if cursor < windowEnd {
			free = append(free, [2]int64{cursor, windowEnd})
		}
		needMs := int64(need) * 60000
		for _, span := range free {
			length := span[1] - span[0]
			if length < needMs {
				continue
			}
			slots = append(slots, scheduleSlot{ID: "p" + strconv.Itoa(len(slots)), Begin: span[0], End: span[0] + needMs})
			if length >= needMs+30*60000 {
				endBegin := span[1] - needMs
				if endBegin != span[0] {
					slots = append(slots, scheduleSlot{ID: "p" + strconv.Itoa(len(slots)), Begin: endBegin, End: span[1]})
				}
			}
			if len(slots) >= 4 {
				break
			}
		}
		if len(slots) > 4 {
			slots = slots[:4]
		}
	}

	chosen := objectOrEmpty(prep["chosenSlot"])
	chosenBegin, chosenEnd := int64(number(chosen["begin"])), int64(number(chosen["end"]))
	chosenLabel := ""
	chosenMissing := false
	if chosenBegin > 0 && chosenEnd > chosenBegin {
		for _, slot := range slots {
			if slot.Begin == chosenBegin && slot.End == chosenEnd {
				chosenLabel = slotText(slot.Begin, slot.End, loc)
				break
			}
		}
		if chosenLabel == "" {
			chosenMissing = true
		}
	}

	var summary strings.Builder
	if expired {
		summary.WriteString(fmt.Sprintf("准备窗口 %s 已经过去，这次没有可用的空档。", rangeText(windowBegin, windowEnd, loc)))
	} else if len(critical) == 0 {
		summary.WriteString(fmt.Sprintf("准备窗口 %s，需要 %d 分钟。", rangeText(windowBegin, windowEnd, loc), need))
		if effectiveBegin > windowBegin {
			summary.WriteString(fmt.Sprintf("现在是 %s，只算这之后的空档。", now.In(loc).Format("15:04")))
		}
		for _, line := range changes {
			summary.WriteString(line)
		}
		if len(conflicts) > 0 {
			for _, line := range conflicts {
				summary.WriteString(line)
			}
			summary.WriteString("只检查了你授权的这几个日程，其他占用未知。")
		} else {
			summary.WriteString("在本次已提供范围内未见冲突，其他占用未知。")
		}
		if len(slots) > 0 {
			labels := make([]string, 0, len(slots))
			for _, slot := range slots {
				labels = append(labels, slotText(slot.Begin, slot.End, loc))
			}
			summary.WriteString(fmt.Sprintf("可用 %d 分钟空档：%s。", need, strings.Join(labels, "、")))
		} else {
			summary.WriteString(fmt.Sprintf("在 %s 内没有找到 %d 分钟空档。", rangeText(windowBegin, windowEnd, loc), need))
		}
		if len(materials) > 0 {
			names := make([]string, 0, len(materials))
			for _, m := range materials {
				names = append(names, "「"+str(m, "text")+"」（"+str(m, "status")+"）")
			}
			summary.WriteString("可引用：" + strings.Join(names, "、") + "。")
		}
		if len(methods) > 0 {
			summary.WriteString("你确认过的做法：" + strings.Join(methods, "、") + "。")
		}
		if len(slots) > 0 && len(methods) > 0 {
			summary.WriteString(fmt.Sprintf("建议先看材料，在 %s 做第一段。", slotText(slots[0].Begin, slots[0].End, loc)))
		}
	} else {
		summary.WriteString("这份准备还缺关键信息，先不计算空档。")
	}
	systemSummary := summary.String()

	userText := strings.TrimSpace(str(prep, "text"))
	// 旧存档可能把未改动的系统摘要存在 text 里。没有 textMode 时，只要它
	// 和 computedSummary 相同，就仍按系统内容处理，不能当成用户原话。
	if mode := str(prep, "textMode"); mode == "system" || (mode == "" && userText != "" && userText == strings.TrimSpace(str(prep, "computedSummary"))) {
		userText = ""
	}
	resultText, resultKind := systemSummary, "system"
	if userText != "" {
		resultText, resultKind = userText, "user"
		// 这是当前生成器认定并展示过的用户文字；记下来源，下一次重算或
		// 撤回时才能把它和系统摘要分开，不会当成无法辨认的旧副本藏掉。
		prep["textMode"] = "user"
	}
	changeText := ""
	if userText != "" {
		old := strings.TrimSpace(str(prep, "computedSummary"))
		// 失效卡清理时旧摘要被删掉了，只剩这个布尔标记；它不含原文，
		// 但足以说明这次重算和用户改过的版本不是同一份。
		summaryChanged := prep["summaryChanged"] == true
		if summaryChanged || (old != "" && old != systemSummary) {
			changeText = "系统重算后的结果和你改过的版本不同：" + truncate(systemSummary, 240)
		}
	}
	delete(prep, "summaryChanged")

	if expired {
		questions = append(questions, "这次准备的窗口已经过去，要换一个新的时间范围吗？")
	}
	if len(missing) > 0 {
		for _, item := range missing {
			questions = append(questions, item+"，要换一条吗？")
		}
	}
	if len(methods) == 0 {
		questions = append(questions, "这次准备用哪个已确认的做法？")
	}
	if len(slots) == 0 && len(critical) == 0 && !expired {
		questions = append(questions, "要不要换一个准备窗口？")
	}
	if chosenMissing {
		questions = append(questions, fmt.Sprintf("你之前选的 %s 现在不在可用空档里，要换一个吗？", slotText(chosenBegin, chosenEnd, loc)))
	}
	questions = dedupeStrings(append(critical, questions...))
	question := "这份准备按现在的时间安排可以吗？"
	if len(questions) > 0 {
		question = questions[0]
	}

	status := "ready"
	if len(critical) > 0 || expired {
		status = "pending"
	}
	fp := schedulePlanFingerprint(goal, busy, materials, methods, questions, slots, chosenBegin, chosenEnd, userText, systemSummary, synthetic, status)
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

	// 保存计算结果，供下次比较；用户文本和选择不被覆盖。
	// 同时留下上一版摘要，清理旧副本时才能认出它不是用户写的。
	// 依据已经找不到时，旧摘要里可能还留着已撤回材料的原文；它不再有
	// 核对用途，不能继续随存档保存。
	if len(missing) > 0 {
		delete(prep, "summaryHistory")
		delete(prep, "computedSummary")
	}
	if old := strings.TrimSpace(str(prep, "computedSummary")); old != "" && old != systemSummary {
		history := stringList(prep["summaryHistory"])
		next := []string{old}
		for _, item := range history {
			if item != old && item != systemSummary && len(next) < 5 {
				next = append(next, item)
			}
		}
		rows := make([]any, len(next))
		for i, item := range next {
			rows[i] = item
		}
		prep["summaryHistory"] = rows
	}
	prep["computedSummary"] = systemSummary
	prep["computedAt"] = now.UTC().Format(time.RFC3339)
	goal["preparation"] = prep

	why := "这份准备从你授权的日程和材料里算出来，不改动日历，也不替你决定。"
	if expired {
		why = "这次准备的窗口已经过去，不再推荐旧时段。换一个新的时间范围再算。"
	} else if len(critical) > 0 {
		why = "这份准备还缺关键信息，先问清楚再算，不编造空档。"
	} else if len(missing) > 0 {
		why = "这份准备有一项依据已经找不到，先按现有日程算，材料需要你确认。"
	}
	xml := schedulePlanXML(cardID, title, why, status, synthetic, windowBegin, windowEnd, need, conflicts, changes, slots, chosenBegin, chosenEnd, chosenLabel, materials, methods, questions, resultText, resultKind, changeText, question, provenance, loc)
	proactive := object{
		"kind": "schedule-plan", "key": key, "createdAt": now.UTC().Format(time.RFC3339),
		"fingerprint": fp, "inputs": preparationInputsFingerprint(goal, sources, facts, discoveries),
		"basis": toAnyBasis(basis), "status": status,
	}
	if updated {
		proactive["updated"] = true
	}
	card := object{
		"id": cardID, "date": now.Format("2006-01-02"), "type": "prepared", "position": 0,
		"xml": xml, "reportID": "", "reportTitle": "", "reportSummary": "", "reportColorRamp": "",
		"reportCreatedAt": "", "reportGoalSlug": "", "goalId": id,
		"proactive": proactive,
	}
	return card, true
}

func schedulePlanXML(cardID, title, why, status string, synthetic bool, windowBegin, windowEnd int64, need int, conflicts, changes []string, slots []scheduleSlot, chosenBegin, chosenEnd int64, chosenLabel string, materials []object, methods, questions []string, resultText, resultKind, changeText, question string, provenance []object, loc *time.Location) string {
	var b strings.Builder
	b.WriteString(`<prepared id="` + xmlEscape(cardID) + `" kind="schedule-plan" status="` + xmlEscape(status) + `">`)
	b.WriteString(`<badge>时间准备</badge>`)
	b.WriteString(`<title>` + xmlEscape("「"+title+"」的时间准备") + `</title>`)
	b.WriteString(`<why>` + xmlEscape(why) + `</why>`)
	if synthetic {
		b.WriteString(`<test>合成测试</test>`)
	}
	if windowBegin > 0 && windowEnd > windowBegin {
		b.WriteString(`<window begin="` + strconv.FormatInt(windowBegin, 10) + `" end="` + strconv.FormatInt(windowEnd, 10) + `">` + xmlEscape(rangeText(windowBegin, windowEnd, loc)) + `</window>`)
	}
	if need > 0 {
		b.WriteString(`<need minutes="` + strconv.Itoa(need) + `">` + xmlEscape(strconv.Itoa(need)+" 分钟") + `</need>`)
	}
	if len(changes) > 0 {
		b.WriteString(`<changes>`)
		for _, line := range changes {
			b.WriteString(`<item>` + xmlEscape(line) + `</item>`)
		}
		b.WriteString(`</changes>`)
	}
	for _, line := range conflicts {
		b.WriteString(`<conflict>` + xmlEscape(line) + `</conflict>`)
	}
	if len(slots) > 0 {
		b.WriteString(`<slots>`)
		for _, slot := range slots {
			b.WriteString(`<slot id="` + xmlEscape(slot.ID) + `" begin="` + strconv.FormatInt(slot.Begin, 10) + `" end="` + strconv.FormatInt(slot.End, 10) + `">` + xmlEscape(slotText(slot.Begin, slot.End, loc)) + `</slot>`)
		}
		b.WriteString(`</slots>`)
	}
	if chosenBegin > 0 && chosenEnd > chosenBegin {
		b.WriteString(`<chosen begin="` + strconv.FormatInt(chosenBegin, 10) + `" end="` + strconv.FormatInt(chosenEnd, 10) + `">` + xmlEscape(chosenLabel) + `</chosen>`)
	}
	if len(materials) > 0 {
		b.WriteString(`<materials>`)
		for _, m := range materials {
			b.WriteString(`<item id="` + xmlEscape(str(m, "id")) + `" kind="` + xmlEscape(str(m, "kind")) + `" ref="` + xmlEscape(str(m, "ref")) + `" version="` + strconv.Itoa(int(number(m["version"]))) + `" status="` + xmlEscape(str(m, "status")) + `">`)
			b.WriteString(`<source>` + xmlEscape(str(m, "source")) + `</source>`)
			b.WriteString(`<text>` + xmlEscape(str(m, "text")) + `</text>`)
			b.WriteString(`</item>`)
		}
		b.WriteString(`</materials>`)
	}
	if len(methods) > 0 {
		b.WriteString(`<methods>`)
		for i, method := range methods {
			b.WriteString(`<item id="a` + strconv.Itoa(i) + `">` + xmlEscape(method) + `</item>`)
		}
		b.WriteString(`</methods>`)
	}
	if len(questions) > 0 {
		b.WriteString(`<questions>`)
		for _, q := range questions {
			b.WriteString(`<item>` + xmlEscape(q) + `</item>`)
		}
		b.WriteString(`</questions>`)
	}
	if resultText != "" {
		b.WriteString(`<result kind="` + xmlEscape(resultKind) + `">` + xmlEscape(resultText) + `</result>`)
	}
	if changeText != "" {
		b.WriteString(`<change>` + xmlEscape(changeText) + `</change>`)
	}
	if len(provenance) > 0 {
		b.WriteString(`<provenance>`)
		for _, p := range provenance {
			b.WriteString(`<item kind="` + xmlEscape(str(p, "kind")) + `" id="` + xmlEscape(str(p, "id")) + `" version="` + strconv.Itoa(int(number(p["version"]))) + `">`)
			b.WriteString(`<title>` + xmlEscape(str(p, "title")) + `</title>`)
			b.WriteString(`<range>` + xmlEscape(slotText(int64(number(p["begin"])), int64(number(p["end"])), loc)) + `</range>`)
			b.WriteString(`</item>`)
		}
		b.WriteString(`</provenance>`)
	}
	b.WriteString(`<question>` + xmlEscape(question) + `</question>`)
	b.WriteString(`</prepared>`)
	return b.String()
}
