package main

// 本地主动工作闭环：从用户已经交来的目标、依据和日程里找一件值得推进的事，
// 先做出一份带出处的准备（下一步、清单或选择），再让用户决定。
//
// 设计边界：
//   - 只读本机 dossier，不联网、不调用模型、不发消息、不执行外部动作。
//   - 不新增“事/委托”核心对象；目标、事实、日程各自独立，可以同时有多条线。
//   - 卡片是普通 artifact，沿用原版的按日期卡片协议；没有新页面。
//   - 生成是幂等的：同一天同一目标/日程只有一张卡；依据变化时原地更新并标注。
//   - 用户点“先不做”后进入冷却，不重复打扰。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type proactiveBasis struct {
	Kind    string
	ID      string
	Version int
}

// RunProactive only refreshes explicitly configured snapshots and numeric schedules.
// changed 表示状态被修改（包括把过期卡标成 stale），调用方据此决定是否落盘。
func RunProactive(state object, now time.Time) (made []object, changed bool, err error) {
	return runProactive(state, now, true)
}

// RunProactiveWithGeneration 先执行同一遍失效维护，再按 allowGenerate 决定
// 是否生成。暂停后台准备时仍然必须维护：撤回、更正、过期都要清掉旧依据、
// 旧 XML 和系统派生文案，不能因为暂停就留下失效内容。
func RunProactiveWithGeneration(state object, now time.Time, allowGenerate bool) (made []object, changed bool, err error) {
	return runProactive(state, now, allowGenerate)
}

func runProactive(state object, now time.Time, allowGenerate bool) (made []object, changed bool, err error) {
	if state == nil {
		return nil, false, nil
	}
	goals := objectsOrEmpty(state["goals"])
	facts := objectsOrEmpty(state["facts"])
	sources := objectsOrEmpty(state["sources"])
	discoveries := objectsOrEmpty(state["discoveries"])

	changed = markStalePrepared(state, goals, facts, sources, discoveries, now)
	if !allowGenerate {
		return nil, changed, nil
	}
	artifacts := objectsOrEmpty(state["artifacts"])
	existing := map[string]object{}
	for _, a := range artifacts {
		existing[str(a, "id")] = a
	}
	dismissed := proactiveDismissed(state, now)
	for _, goal := range goals {
		card, ok := buildSchedulePlan(state, goal, sources, facts, discoveries, now, existing, dismissed)
		if !ok {
			continue
		}
		upsertProactiveArtifact(state, card, existing)
		made = append(made, card)
		changed = true
	}
	// 材料简报同样是用户明确配置的成果：先于普通下一步卡，按材料变化
	// 原地重算，不因为当天普通卡名额已满就不生成。
	for _, goal := range goals {
		card, ok := buildMaterialBrief(state, goal, sources, facts, discoveries, now, existing, dismissed)
		if !ok {
			continue
		}
		upsertProactiveArtifact(state, card, existing)
		made = append(made, card)
		changed = true
	}
	return made, changed, nil
}


func objectsOrEmpty(v any) []object {
	rows, err := objects(v)
	if err != nil {
		return nil
	}
	return rows
}

func objectOrEmpty(v any) object {
	o, _ := v.(object)
	if o == nil {
		return object{}
	}
	return o
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func latestEntry(entries []object) object {
	var best object
	var bestAt time.Time
	for _, entry := range entries {
		at, err := time.Parse(time.RFC3339, str(entry, "at"))
		if err != nil {
			continue
		}
		if best == nil || at.After(bestAt) {
			best, bestAt = entry, at
		}
	}
	if best == nil && len(entries) > 0 {
		return entries[len(entries)-1]
	}
	return best
}

func findFact(facts []object, id string) object {
	for _, fact := range facts {
		if str(fact, "id") == id {
			return fact
		}
	}
	return nil
}

func findGoal(goals []object, id string) object {
	for _, goal := range goals {
		if str(goal, "id") == id {
			return goal
		}
	}
	return nil
}

func findSource(sources []object, id string) object {
	for _, source := range sources {
		if str(source, "id") == id {
			return source
		}
	}
	return nil
}

func findDiscovery(discoveries []object, id string) object {
	for _, discovery := range discoveries {
		if str(discovery, "id") == id {
			return discovery
		}
	}
	return nil
}

// A discovery is the adviser's inference or a user correction, never a
// user-confirmed fact. Keep that distinction visible on every card.
func discoverySourceTitle(discovery object) string {
	if str(discovery, "status") == "corrected" {
		return "本人更正的说法"
	}
	if str(discovery, "kind") == "question" {
		return "顾问提出的问题"
	}
	return "顾问提出的判断"
}

func factSourceTitle(state object, fact object) string {
	title := ""
	sourceID := str(fact, "sourceId")
	if sourceID == "" {
		title = str(fact, "source")
		if title == "" {
			if fact["confirmed"] == true {
				title = "本人确认"
			} else {
				title = "未确认"
			}
		}
	} else if source := findSource(objectsOrEmpty(state["sources"]), sourceID); source != nil {
		title = strings.TrimSpace(str(source, "title"))
	}
	if title == "" {
		title = "你的资料"
	}
	if fact["confirmed"] != true {
		title += "（未确认）"
	}
	return title
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// goalFingerprint mirrors the inputs buildGoalCard uses. It lets the stale
// pass notice progress, a changed step/method, or a changed basis even when
// the card is answered or the goal is too fresh to rebuild.
func goalFingerprint(goal object, facts, discoveries []object) string {
	title := strings.TrimSpace(str(goal, "title"))
	step := strings.TrimSpace(str(goal, "step"))
	methods := []string{}
	for _, method := range objectsOrEmpty(goal["approvedMethods"]) {
		if text := strings.TrimSpace(str(method, "text")); text != "" {
			methods = append(methods, text)
		}
	}
	entries := objectsOrEmpty(goal["entries"])
	var last object
	if len(entries) > 0 {
		last = latestEntry(entries)
	}
	basis := []proactiveBasis{}
	for _, ref := range objectsOrEmpty(goal["approvedBasis"]) {
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
			if fact := findFact(facts, id); fact != nil {
				basis = append(basis, proactiveBasis{Kind: "fact", ID: id, Version: int(number(fact["version"]))})
			}
		case "discovery":
			if discovery := findDiscovery(discoveries, id); discovery != nil && str(discovery, "status") != "stale" {
				basis = append(basis, proactiveBasis{Kind: "discovery", ID: id, Version: int(number(discovery["version"]))})
			}
		}
	}
	return fingerprint(title, step, strings.Join(methods, "|"), lastEntryID(last), basisFingerprint(basis))
}

func basisFingerprint(basis []proactiveBasis) string {
	parts := make([]string, len(basis))
	for i, item := range basis {
		parts[i] = fmt.Sprintf("%s:%s:%d", item.Kind, item.ID, item.Version)
	}
	return strings.Join(parts, ",")
}

func toAnyBasis(basis []proactiveBasis) []any {
	out := make([]any, len(basis))
	for i, item := range basis {
		out[i] = object{"kind": item.Kind, "id": item.ID, "version": item.Version}
	}
	return out
}

func toAnyObjects(rows []object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

func lastEntryID(last object) string {
	if last == nil {
		return ""
	}
	return str(last, "id") + "@" + str(last, "at")
}

func proactiveDismissed(state object, now time.Time) map[string]bool {
	out := map[string]bool{}
	proactive := objectOrEmpty(state["proactive"])
	for _, row := range objectsOrEmpty(proactive["dismissed"]) {
		key := str(row, "key")
		until, err := time.Parse(time.RFC3339, str(row, "until"))
		if key != "" && err == nil && until.After(now) {
			out[key] = true
		}
	}
	return out
}

func upsertProactiveArtifact(state object, card object, existing map[string]object) {
	artifacts := objectsOrEmpty(state["artifacts"])
	id := str(card, "id")
	for i, old := range artifacts {
		if str(old, "id") != id {
			continue
		}
		if value, ok := old["answered"]; ok {
			card["answered"] = value
		}
		if value, ok := old["dismissed"]; ok {
			card["dismissed"] = value
		}
		if value, ok := old["position"]; ok {
			card["position"] = value
		}
		// A rebuilt card replaces the old XML, but the user's own choice and
		// edit must survive. A legacy copy with no provenance is never copied
		// as text; it stays in the quarantine branch below and is not rendered.
		if oldPlan := objectOrEmpty(old["plan"]); len(oldPlan) > 0 {
			plan := objectOrEmpty(card["plan"])
			if len(plan) == 0 {
				plan = object{}
			}
			switch str(oldPlan, "textMode") {
			case "user":
				for _, key := range []string{"text", "textMode", "userNote", "systemCopy", "kind", "textAt", "systemCopyAt", "textSanitized"} {
					if value, ok := oldPlan[key]; ok {
						plan[key] = value
					}
				}
			case "system":
				for _, key := range []string{"textMode", "systemCopy", "systemCopyAt"} {
					if value, ok := oldPlan[key]; ok {
						plan[key] = value
					}
				}
			}
			if value, ok := oldPlan["chosenSlot"]; ok {
				plan["chosenSlot"] = value
			}
			if oldPlan["textNeedsReview"] == true || str(oldPlan, "legacyText") != "" {
				if value, ok := oldPlan["legacyText"]; ok {
					plan["legacyText"] = value
				}
				if oldPlan["textNeedsReview"] == true {
					plan["textNeedsReview"] = true
				}
			}
			if len(plan) > 0 {
				card["plan"] = plan
			}
		}
		artifacts[i] = card
		state["artifacts"] = toAnyObjects(artifacts)
		existing[id] = card
		return
	}
	card["position"] = len(artifacts)
	artifacts = append(artifacts, card)
	state["artifacts"] = toAnyObjects(artifacts)
	existing[id] = card
}

func setPreparedStatus(xml, status string) string {
	const prefix = `status="`
	i := strings.Index(xml, prefix)
	if i < 0 {
		return xml
	}
	start := i + len(prefix)
	j := strings.Index(xml[start:], `"`)
	if j < 0 {
		return xml
	}
	return xml[:start] + status + xml[start+j:]
}

// stalePreparedXML replaces every derived part of a card with a short notice.
// A stale card must not keep the private quote, source, steps or choices that
// were derived from a withdrawn basis.
func stalePreparedXML(artifact object) string {
	proactive := objectOrEmpty(artifact["proactive"])
	kind := str(proactive, "kind")
	if kind == "" {
		kind = "prepared"
	}
	return `<prepared id="` + xmlEscape(str(artifact, "id")) + `" kind="` + xmlEscape(kind) + `" status="stale">` +
		`<badge>已失效</badge><title>这份准备已失效</title>` +
		`<why>依据已撤回或发生变化，不再显示原来的内容。</why>` +
		`<question>这份准备已经不能用了。</question></prepared>`
}

// stripSystemSentences keeps the parts of a saved preparation that the system
// summary did not already contain. Those are the sentences the user actually
// added or rewrote; unchanged system sentences are dropped.
func stripSystemSentences(systemText, userText string) string {
	systemText = strings.TrimSpace(systemText)
	userText = strings.TrimSpace(userText)
	if userText == "" {
		return ""
	}
	if systemText == "" {
		return userText
	}
	var kept strings.Builder
	start := 0
	flush := func(end int) {
		part := strings.TrimSpace(userText[start:end])
		start = end
		bare := strings.TrimRight(part, "。；!?！？")
		if bare == "" || strings.Contains(systemText, bare) {
			return
		}
		kept.WriteString(part)
	}
	for i, r := range userText {
		if r == '。' || r == '；' || r == '!' || r == '?' || r == '！' || r == '？' || r == '\n' {
			flush(i + len(string(r)))
		}
	}
	flush(len(userText))
	return strings.TrimSpace(kept.String())
}

// sanitizePreparedText removes a saved system copy from the artifact and the
// goal preparation when the card goes stale. Genuine user edits survive; a
// legacy copy that cannot be classified is kept aside and marked for review.
func sanitizePreparedText(artifact, goal object) bool {
	plan := objectOrEmpty(artifact["plan"])
	// A material brief keeps its plan fields on goal.brief, not on
	// goal.preparation; both use the same textMode/userNote protocol.
	configKey := "preparation"
	if str(objectOrEmpty(artifact["proactive"]), "kind") == "material-brief" {
		configKey = "brief"
	}
	prep := objectOrEmpty(goal[configKey])
	if len(plan) == 0 && len(prep) == 0 {
		return false
	}
	systemTexts := []string{}
	if v := strings.TrimSpace(str(prep, "computedSummary")); v != "" {
		systemTexts = append(systemTexts, v)
	}
	for _, v := range stringList(prep["summaryHistory"]) {
		if v != "" {
			systemTexts = append(systemTexts, v)
		}
	}
	if v := strings.TrimSpace(str(plan, "systemCopy")); v != "" {
		systemTexts = append(systemTexts, v)
	}
	systemText := strings.Join(systemTexts, "\n")
	changed := false
	clean := func(holder object) bool {
		if len(holder) == 0 {
			return false
		}
		text := strings.TrimSpace(str(holder, "text"))
		if text == "" {
			if _, ok := holder["text"]; ok {
				delete(holder, "text")
				return true
			}
			return false
		}
		switch str(holder, "textMode") {
		case "system":
			delete(holder, "text")
			return true
		case "user":
			note := strings.TrimSpace(str(holder, "userNote"))
			if note == "" {
				note = stripSystemSentences(systemText, text)
			}
			if note != "" {
				holder["text"] = note
			} else {
				delete(holder, "text")
			}
			if holder["textSanitized"] != true {
				holder["textSanitized"] = true
				return true
			}
			return note != text
		default:
			if note := strings.TrimSpace(str(holder, "userNote")); note != "" {
				holder["text"] = note
				holder["textMode"] = "user"
				holder["textSanitized"] = true
				return true
			}
			for _, sys := range systemTexts {
				if text == sys {
					delete(holder, "text")
					holder["textMode"] = "system"
					return true
				}
			}
			// 没有来源标记的旧副本不能靠“没匹配上系统句”推断成用户原创。
			// 整段停用显示，保留可恢复副本，等用户核对。
			holder["legacyText"] = text
			holder["textNeedsReview"] = true
			delete(holder, "text")
			return true
		}
	}
	if clean(plan) {
		changed = true
	}
	if _, ok := plan["systemCopy"]; ok {
		delete(plan, "systemCopy")
		changed = true
	}
	if clean(prep) {
		changed = true
	}
	if _, ok := prep["systemCopy"]; ok {
		delete(prep, "systemCopy")
		changed = true
	}
	// 旧摘要只用于识别系统句；卡已失效后不再保留，避免已撤回材料的原文
	// 继续留在存档里。重算时会写入新的 computedSummary。只留一个布尔标记，
	// 让下一次重算仍能提示“系统结果和你改过的版本不同”，又不保存旧原文。
	if _, ok := prep["computedSummary"]; ok {
		delete(prep, "computedSummary")
		prep["summaryChanged"] = true
		changed = true
	}
	if _, ok := prep["summaryHistory"]; ok {
		delete(prep, "summaryHistory")
		changed = true
	}
	if changed {
		artifact["plan"] = plan
		if len(goal) > 0 {
			goal[configKey] = prep
		}
	}
	return changed
}

func markStalePrepared(state object, goals, facts, sources, discoveries []object, now time.Time) bool {
	changed := false
	artifacts := objectsOrEmpty(state["artifacts"])
	for _, artifact := range artifacts {
		if str(artifact, "type") != "prepared" {
			continue
		}
		proactive := objectOrEmpty(artifact["proactive"])
		if str(proactive, "status") == "stale" {
			// Only a stale card loses its derived text. Sanitizing a live card
			// would misread the producer's current user text as an unknown
			// legacy copy and hide it on the next tick.
			if sanitizePreparedText(artifact, findGoal(goals, str(artifact, "goalId"))) {
				changed = true
			}
			// Older builds only changed the status attribute and left the
			// private quote in the XML. Replace any non-canonical stale XML so
			// no derived quote, source, slot or material survives.
			if str(artifact, "xml") != stalePreparedXML(artifact) {
				artifact["xml"] = stalePreparedXML(artifact)
				changed = true
			}
			if _, ok := proactive["basis"]; ok {
				delete(proactive, "basis")
				artifact["proactive"] = proactive
				changed = true
			}
			continue
		}
		key := str(proactive, "key")
		stale := false
		switch str(proactive, "kind") {
		case "goal-next-step":
			goal := findGoal(goals, strings.TrimPrefix(key, "goal:"))
			if goal == nil || str(goal, "step") == "已达成收尾" {
				stale = true
			} else if str(proactive, "fingerprint") != goalFingerprint(goal, facts, discoveries) {
				stale = true
			}
		case "schedule-plan":
			goal := findGoal(goals, strings.TrimPrefix(key, "plan:"))
			if goal == nil || str(goal, "step") == "已达成收尾" || len(objectOrEmpty(goal["preparation"])) == 0 {
				stale = true
			}
			// 昨天的空档不能留成 ready 可执行状态；用户原先的选择仍留在
			// preparation 里，只把卡降为只读历史。
			if date := str(artifact, "date"); date != "" && date != now.Format("2006-01-02") {
				stale = true
			}
			if goal != nil {
				prep := objectOrEmpty(goal["preparation"])
				if prep["ignored"] == true {
					stale = true
				}
				// 来源没有正式 version 契约时，版本号不变也可能改了原文或
				// 时间。输入指纹核对实际内容，已回答的卡同样要失效。
				if stored := str(proactive, "inputs"); stored != "" {
					if stored != preparationInputsFingerprint(goal, sources, facts, discoveries) {
						stale = true
					}
				} else if artifact["answered"] != nil {
					// 旧版本的卡没有输入指纹，无法核对内容；已回答的卡
					// 不再可执行，降为只读历史。
					stale = true
				}
			}
		case "material-brief":
			goal := findGoal(goals, strings.TrimPrefix(key, "brief:"))
			if goal == nil || str(goal, "step") == "已达成收尾" {
				stale = true
			} else {
				brief := objectOrEmpty(goal["brief"])
				if len(brief) == 0 || brief["ignored"] == true {
					stale = true
				} else if refs := objectsOrEmpty(brief["materialRefs"]); len(refs) == 0 {
					stale = true
				} else {
					// 撤掉一部分材料时只重算剩下的内容，不把整张卡判死；
					// 只有所有材料都不在了才失效。
					available := 0
					for _, ref := range refs {
						switch str(ref, "kind") {
						case "fact":
							if findFact(facts, str(ref, "id")) != nil {
								available++
							}
						case "discovery":
							discovery := findDiscovery(discoveries, str(ref, "id"))
							if discovery != nil && str(discovery, "status") != "stale" {
								available++
							}
						case "source":
							source := findSource(sources, str(ref, "id"))
							if source != nil && str(objectOrEmpty(source["calendarChange"]), "kind") != "cancelled" {
								available++
							}
						default:
							available++
						}
					}
					if available == 0 {
						stale = true
					}
				}
			}
		case "event-prep":
			source := findSource(sources, strings.TrimPrefix(key, "event:"))
			if source == nil {
				stale = true
			} else {
				calendar := objectOrEmpty(source["calendar"])
				change := objectOrEmpty(source["calendarChange"])
				if str(change, "kind") == "cancelled" {
					stale = true
				}
				if after := objectOrEmpty(change["after"]); len(after) > 0 {
					calendar = after
				}
				begin, end := number(calendar["begin"]), number(calendar["end"])
				if end > 0 && time.UnixMilli(int64(end)).Before(now) {
					stale = true
				}
				if begin > 0 && time.UnixMilli(int64(begin)).After(now.Add(48*time.Hour)) {
					stale = true
				}
				if storedBegin, storedEnd := number(proactive["begin"]), number(proactive["end"]); storedBegin > 0 && (storedBegin != begin || storedEnd != end) {
					stale = true
				}
			}
			if goalID := str(artifact, "goalId"); goalID != "" {
				goal := findGoal(goals, goalID)
				if goal == nil || str(proactive, "goalFingerprint") != goalFingerprint(goal, facts, discoveries) {
					stale = true
				}
			}
		}
		// A card's own basis is authoritative. A deleted fact/source, a
		// corrected version or a withdrawn discovery invalidates the card even
		// if the goal and event still exist. A material brief is the exception:
		// one withdrawn material only removes its own part, so the builder
		// recomputes from the remaining materials and the card stays usable.
		if str(proactive, "kind") != "material-brief" {
			for _, ref := range objectsOrEmpty(proactive["basis"]) {
				switch str(ref, "kind") {
				case "fact":
					fact := findFact(facts, str(ref, "id"))
					if fact == nil {
						stale = true
						continue
					}
					accepted := int(number(ref["version"]))
					if accepted > 0 && int(number(fact["version"])) != accepted {
						stale = true
					}
				case "source":
					source := findSource(sources, str(ref, "id"))
					if source == nil || str(objectOrEmpty(source["calendarChange"]), "kind") == "cancelled" {
						stale = true
					}
				case "discovery":
					discovery := findDiscovery(discoveries, str(ref, "id"))
					if discovery == nil || str(discovery, "status") == "stale" {
						stale = true
						continue
					}
					accepted := int(number(ref["version"]))
					if accepted > 0 && int(number(discovery["version"])) != accepted {
						stale = true
					}
				}
			}
		}
		if !stale {
			continue
		}
		if sanitizePreparedText(artifact, findGoal(goals, str(artifact, "goalId"))) {
			changed = true
		}
		proactive["status"] = "stale"
		delete(proactive, "basis")
		artifact["proactive"] = proactive
		artifact["xml"] = stalePreparedXML(artifact)
		changed = true
	}
	if changed {
		state["artifacts"] = toAnyObjects(artifacts)
	}
	return changed
}
