package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	firstReadingSpaces    = regexp.MustCompile(`[ \t\r\n\f\v]+`)
	firstReadingForwarded = regexp.MustCompile(`(?im)^(?:-{2,}\s*(?:original message|forwarded message|转发邮件)|On [^\n]+ wrote:\s*$|在[^\n]{1,150}写道[：:]\s*$)`)
	firstReadingQualifier = regexp.MustCompile(`(?i)^(?:但|不过|除非|前提|条件是|也可能|还(?:没|没有|未)(?:最后)?决定|并没|其实没|but\b|unless\b|only if\b|provided\b|that depends\b|I haven['’]t decided\b)`)
	firstReadingAuthor    = regexp.MustCompile(`^我（([^）\n]*)）：`)
	firstReadingDate      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z)?$`)
	firstReadingField     = regexp.MustCompile(`^([\pL][\pL ]{0,23})[:：]`)
	firstReadingTime      = regexp.MustCompile(`^(?:我)?(?:(?:下|本|这)?(?:周|星期)[一二三四五六日天]|明天|今天|后天)(?:上午|下午|早上|早晨|中午|晚上)?[ \t\r\n\f\v\x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*(?:[一二两三四五六七八九十〇零\d]+[点时]|\d{1,2}[:：]\d{2})`)
)

// Legacy evidence-suppression compatibility only. These grammars must not
// be used to infer new facts: old forgotten ranges still need matching hashes.
const maxNoteFields = 6

var (
	fieldSeparator     = regexp.MustCompile(`[;；]`)
	clauseSeparator    = regexp.MustCompile(`[,，;；]`)
	ambiguousStatement = regexp.MustCompile(`[“”「」『』"？?\n]|如果|假如|假设|要是|只要|除非|倘若|的话|才会|也许|可能`)
	personalIntention  = regexp.MustCompile(`^我(?:原本|本来|现在|曾经|之前|当时)?(?:想|希望|打算|计划|准备|考虑)`)
)

// This fingerprint is a scoped suppression identifier, not anonymisation. Match
// the browser's versioned UTF-8/ASCII-whitespace contract exactly.
func firstReadingHash(kind, text string) string {
	normalized := strings.Trim(firstReadingSpaces.ReplaceAllString(text, " "), " ")
	return fmt.Sprintf("%x", sha256.Sum256([]byte("quest:first-reading:"+kind+":v1\x00"+normalized)))
}

func firstReadingJSSpace(r rune) bool {
	return strings.ContainsRune(" \t\r\n\f\v\u00a0\u1680\u2028\u2029\u202f\u205f\u3000\ufeff", r) || r >= '\u2000' && r <= '\u200a'
}

func firstReadingTrim(s string) string { return strings.TrimFunc(s, firstReadingJSSpace) }

type firstReadingPassage struct {
	start, end int
	kind, date string
	components []firstReadingPassage
}

func firstReadingSentences(text string, offset int) []firstReadingPassage {
	var spans []firstReadingPassage
	start, depth := 0, 0
	quoted := false
	for i, r := range text {
		next := i + utf8.RuneLen(r)
		switch {
		case strings.ContainsRune("“「『", r):
			depth++
		case strings.ContainsRune("”」』", r) && depth > 0:
			depth--
		case r == '"':
			quoted = !quoted
		}
		if depth > 0 || quoted {
			continue
		}
		if strings.ContainsRune("\n。！？!?", r) || r == '.' && (next == len(text) || firstReadingJSSpace(firstRune(text[next:]))) {
			spans = append(spans, firstReadingPassage{start: offset + start, end: offset + next})
			start = next
		}
	}
	if start < len(text) {
		spans = append(spans, firstReadingPassage{start: offset + start, end: offset + len(text)})
	}
	return spans
}

// Field labels contain only letters and spaces. Match JavaScript's default
// lowercase for dotted I and contextual final sigma as well as simple mappings;
// otherwise a whole forgotten field group could be split differently on reload.
func firstReadingLowerLabel(text string) string {
	runes := []rune(text)
	cased := func(r rune) bool {
		return unicode.IsLower(r) || unicode.IsUpper(r) || unicode.IsTitle(r) ||
			unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
	}
	nearbyCased := func(at, step int) bool {
		for at >= 0 && at < len(runes) {
			if cased(runes[at]) {
				return true
			}
			// Of JavaScript's Case_Ignorable characters, only modifier letters
			// can occur in the already validated letter/space label grammar.
			if !unicode.Is(unicode.Lm, runes[at]) {
				return false
			}
			at += step
		}
		return false
	}
	var lower strings.Builder
	for i, r := range runes {
		switch {
		case r == 'İ':
			lower.WriteString("i\u0307")
		case r == 'Σ' && nearbyCased(i-1, -1) && !nearbyCased(i+1, 1):
			lower.WriteRune('ς')
		default:
			lower.WriteRune(unicode.ToLower(r))
		}
	}
	return lower.String()
}

// The existing browser independentStatements contract counts a single astral
// character as two UTF-16 units. Preserve its exact split when hashing passages.
func firstReadingIndependent(kind, text string) []string {
	if kind != "self" || ambiguousStatement.MatchString(text) {
		return nil
	}
	fields := fieldSeparator.Split(text, -1)
	if len(fields) > 1 && len(fields) <= maxNoteFields {
		labels := map[string]bool{}
		for i, part := range fields {
			fields[i] = firstReadingTrim(part)
			match := firstReadingField.FindStringSubmatch(fields[i])
			if match == nil || firstReadingTrim(strings.TrimPrefix(fields[i], match[0])) == "" {
				break
			}
			labels[firstReadingLowerLabel(firstReadingTrim(match[1]))] = true
		}
		if len(labels) == len(fields) {
			return fields
		}
	}
	time := firstReadingTime.FindString(text)
	clauses := clauseSeparator.Split(text, -1)
	if time == "" || len(clauses) != 2 {
		return nil
	}
	first, second := firstReadingTrim(clauses[0]), firstReadingTrim(clauses[1])
	action := firstReadingTrim(strings.TrimPrefix(first, time))
	units, onlyTime := 0, true
	for _, r := range action {
		units++
		if r > 0xffff {
			units++
		}
		if !strings.ContainsRune("零〇一二两三四五六七八九十0123456789分刻半钟整点前后左右上下大约过刚的时候", r) && !firstReadingJSSpace(r) {
			onlyTime = false
		}
	}
	if units < 2 || onlyTime || !personalIntention.MatchString(second) || strings.ContainsAny(first, "。.!！\n") {
		return nil
	}
	for i, r := range second {
		if strings.ContainsRune("。.!！\n", r) && i+utf8.RuneLen(r) < len(second) {
			return nil
		}
	}
	return []string{first, second}
}

func firstReadingPassages(src Source) []firstReadingPassage {
	text := src.Text
	limit := len(text)
	if match := firstReadingForwarded.FindStringIndex(text); match != nil {
		limit = match[0]
	}
	var spans []firstReadingPassage
	if src.Origin == "ai-export" {
		offset := 0
		for _, line := range strings.Split(text[:limit], "\n") {
			owner := firstReadingAuthor.FindStringSubmatch(line)
			if owner != nil {
				date := owner[1]
				if date != "日期未注明" && !firstReadingDate.MatchString(date) {
					date = ""
				}
				for _, span := range firstReadingSentences(line[len(owner[0]):], offset+len(owner[0])) {
					span.kind, span.date = "self", date
					spans = append(spans, span)
				}
			}
			offset += len(line) + 1
		}
	} else {
		spans = firstReadingSentences(text[:limit], 0)
		for i := range spans {
			spans[i].kind = src.Kind
		}
	}
	var out []firstReadingPassage
	for i := 0; i < len(spans); i++ {
		span := spans[i]
		components := []firstReadingPassage{span}
		for i+1 < len(spans) {
			next := spans[i+1]
			if next.kind != span.kind || next.date != span.date || firstReadingTrim(text[span.end:next.start]) != "" ||
				!firstReadingQualifier.MatchString(firstReadingTrim(text[next.start:next.end])) {
				break
			}
			components = append(components, next)
			span.end = next.end
			i++
		}
		quote := firstReadingTrim(text[span.start:span.end])
		if quote == "" {
			continue
		}
		span.start += strings.Index(text[span.start:span.end], quote)
		span.end = span.start + len(quote)
		lineStart := strings.LastIndexByte(text[:span.start], '\n') + 1
		if strings.HasPrefix(strings.TrimLeftFunc(text[lineStart:span.end], firstReadingJSSpace), ">") {
			continue
		}
		if parts := firstReadingIndependent(span.kind, quote); len(parts) > 0 {
			from := span.start
			for _, part := range parts {
				at := from + strings.Index(text[from:], part)
				out = append(out, firstReadingPassage{start: at, end: at + len(part), kind: span.kind, date: span.date})
				from = at + len(part)
			}
		} else {
			if len(components) > 1 {
				span.components = components
			}
			out = append(out, span)
		}
	}
	return out
}

type oldRange struct{ start, end int }

func androidRememberReadingSuppressions(tx *sql.Tx, user string, src Source) error {
	var raw sql.NullString
	if err := tx.QueryRow("SELECT metadata FROM android_dossier WHERE user_id=?", user).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !raw.Valid {
		return nil
	}
	metadata, err := parseObject([]byte(raw.String))
	if err != nil {
		return err
	}
	var ranges []oldRange
	rows, err := tx.Query("SELECT start_byte,end_byte FROM dismissed WHERE user_id=? AND source_id=?", user, src.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var span oldRange
		if err := rows.Scan(&span.start, &span.end); err != nil {
			rows.Close()
			return err
		}
		ranges = append(ranges, span)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.Query("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND confirmed=1", user)
	if err != nil {
		return err
	}
	for rows.Next() {
		memory, history, err := readMemory(rows)
		if err != nil {
			rows.Close()
			return err
		}
		if len(history) == 0 {
			continue
		}
		for _, span := range memory.Evidence {
			if span.SourceID == src.ID {
				ranges = append(ranges, oldRange{span.Start, span.End})
			}
		}
		if len(memory.Evidence) == 0 && memory.SourceID == src.ID && memory.Quote != "" {
			if at := strings.Index(src.Text, memory.Quote); at >= 0 {
				ranges = append(ranges, oldRange{at, at + len(memory.Quote)})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ranges) == 0 {
		return err
	}
	retained, _ := objects(metadata["readingSuppressions"])
	known := map[string]bool{}
	for _, ref := range retained {
		if version, _ := integer(ref["version"]); version == 1 {
			known[str(ref, "documentHash")+":"+str(ref, "passageHash")] = true
		}
	}
	document := firstReadingHash("document", src.Text)
	remember := func(quote string) {
		if strings.Trim(firstReadingSpaces.ReplaceAllString(quote, " "), " ") == "" {
			return
		}
		passage := firstReadingHash("passage", quote)
		if !known[document+":"+passage] {
			retained = append(retained, object{"version": 1, "documentHash": document, "passageHash": passage})
			known[document+":"+passage] = true
		}
	}
	passages := firstReadingPassages(src)
	for _, span := range ranges {
		if span.start < 0 || span.end <= span.start || span.end > len(src.Text) || !utf8.ValidString(src.Text[span.start:span.end]) {
			continue
		}
		remember(src.Text[span.start:span.end])
		for _, passage := range passages {
			if passage.start < span.end && span.start < passage.end {
				remember(src.Text[passage.start:passage.end])
				for _, component := range passage.components {
					if component.start < span.end && span.start < component.end {
						remember(src.Text[component.start:component.end])
					}
				}
			}
		}
	}
	metadata["readingSuppressions"] = retained
	_, err = tx.Exec("UPDATE android_dossier SET metadata=? WHERE user_id=?", encode(metadata), user)
	return err
}

// Discovery references identify the generated result independently of its raw
// citations. They retain no extra copy of its title/body. A stale reference is
// kept only as a withdrawal index; it must never be resolved as current text.
func firstReadingDiscoveryReferences(message object) []object {
	refs, _ := objects(message["discoveryReferences"])
	return refs
}

func firstReadingLegacyDiscovery(message object) bool {
	if len(firstReadingDiscoveryReferences(message)) > 0 {
		return false
	}
	refs, _ := objects(message["readingReferences"])
	for _, ref := range refs {
		if str(ref, "via") == "proposed-opening" {
			return true
		}
	}
	if len(refs) == 0 {
		return false
	}
	keys, _ := message["referenceKeys"].([]any)
	for _, key := range keys {
		if key == "opening" {
			return true
		}
	}
	return false
}

func firstReadingRevision(value any) (int, error) {
	if value == nil {
		return 0, nil
	}
	return integer(value)
}

// Compare a citation set by explicit fields, not map/object serialization order.
// Unknown display fields are not a new authority for the cited passage.
func firstReadingSameCitations(left, right any) bool {
	a, err := objects(left)
	if err != nil || len(a) == 0 {
		return false
	}
	b, err := objects(right)
	if err != nil || len(b) == 0 {
		return false
	}
	set := func(refs []object) map[string]bool {
		out := map[string]bool{}
		for _, ref := range refs {
			out[encode([]any{str(ref, "key"), ref["version"], str(ref, "quote"), str(ref, "provenance")})] = true
		}
		return out
	}
	x, y := set(a), set(b)
	if len(a) != len(b) || len(x) != len(a) || len(y) != len(b) {
		return false
	}
	for key := range x {
		if !y[key] {
			return false
		}
	}
	return true
}

func firstReadingDiscoveryCurrent(ref, task object) bool {
	mode := ""
	switch str(task, "status") {
	case "complete":
		mode = "proposed"
	case "corrected":
		mode = "confirmed-discovery"
	default:
		return false
	}
	result, _ := task["result"].(object)
	revision, err := firstReadingRevision(ref["revision"])
	current, currentErr := firstReadingRevision(task["revision"])
	return err == nil && currentErr == nil && str(ref, "id") != "" && str(ref, "id") == str(task, "id") &&
		str(ref, "attemptId") == str(task, "attemptId") && revision == current && str(ref, "mode") == mode &&
		firstReadingSameCitations(ref["citations"], result["citations"])
}

// The Android CAS boundary also checks result identity when the task is
// replaced/retried/corrected. This is a dependency check, not model fact checking
// or a replacement for the browser's full legal-source projection.
func pruneDossierDiscoveryReferences(d *dossierRows) {
	reveal, _ := d.fields["reveal"].(object)
	task, _ := reveal["firstReading"].(object)
	facts := map[string]object{}
	for _, fact := range d.rows["facts"] {
		facts[str(fact, "id")] = fact
	}
	removed := func(ref object) bool {
		id := str(ref, "id")
		if id == "" && strings.HasPrefix(str(ref, "key"), "f:") {
			id = strings.TrimPrefix(str(ref, "key"), "f:")
		}
		if id != "" && facts[id] == nil {
			return true
		}
		source := str(ref, "sourceId")
		if source == "" {
			return false
		}
		if d.sources[source] == nil {
			return true
		}
		start, e1 := integer(ref["start"])
		end, e2 := integer(ref["end"])
		if e1 != nil || e2 != nil || end <= start {
			return false
		}
		for _, span := range d.rows["dismissed"] {
			a, e1 := integer(span["start"])
			b, e2 := integer(span["end"])
			if str(span, "sourceId") == source && e1 == nil && e2 == nil && a < end && start < b {
				return true
			}
		}
		return false
	}
	messages := make([]object, 0, len(d.rows["conversation"]))
	for _, message := range d.rows["conversation"] {
		if str(message, "role") != "assistant" {
			messages = append(messages, message)
			continue
		}
		refs := firstReadingDiscoveryReferences(message)
		legacy := firstReadingLegacyDiscovery(message)
		stale, withdrawn := legacy, false
		for _, ref := range refs {
			stale = stale || !firstReadingDiscoveryCurrent(ref, task)
			citations, _ := objects(ref["citations"])
			for _, citation := range citations {
				withdrawn = withdrawn || removed(citation)
				if fact := facts[str(citation, "id")]; fact != nil {
					v, e1 := integer(citation["version"])
					current, e2 := integer(fact["version"])
					stale = stale || e1 != nil || e2 != nil || v != current
				}
			}
		}
		if legacy {
			citations, _ := objects(message["readingReferences"])
			for _, citation := range citations {
				withdrawn = withdrawn || removed(citation)
			}
		}
		if withdrawn {
			continue
		}
		if stale {
			message["basisCorrected"] = true
		}
		messages = append(messages, message)
	}
	firstReadingPropagateConversation(messages)
	d.rows["conversation"] = messages
}

// Follow only recorded prior-question/answer references. Old clients could lose
// the opening marker while deduplicating its citations against a raw passage.
func firstReadingPropagateConversation(messages []object) {
	byID := map[string]int{}
	for i, message := range messages {
		byID[str(message, "id")] = i
	}
	var current func(object, map[string]bool) bool
	current = func(message object, visiting map[string]bool) bool {
		id := str(message, "id")
		if message["basisCorrected"] == true || firstReadingLegacyDiscovery(message) || visiting[id] {
			return false
		}
		visiting[id] = true
		defer delete(visiting, id)
		keys, _ := message["referenceKeys"].([]any)
		for _, key := range keys {
			text, ok := key.(string)
			if !ok || !(strings.HasPrefix(text, "m:") || strings.HasPrefix(text, "u:")) {
				continue
			}
			i, ok := byID[text[2:]]
			if !ok {
				return false
			}
			if strings.HasPrefix(text, "u:") {
				i++
			}
			if i >= len(messages) || str(messages[i], "role") != "assistant" || !current(messages[i], visiting) {
				return false
			}
		}
		return true
	}
	for _, message := range messages {
		if str(message, "role") == "assistant" && !current(message, map[string]bool{}) {
			message["basisCorrected"] = true
		}
	}
}

func androidPropagateDiscoveryConversation(tx *sql.Tx, user string) error {
	rows, err := tx.Query("SELECT id,extra FROM android_fields WHERE user_id=? AND kind='conversation' ORDER BY position,id", user)
	if err != nil {
		return err
	}
	var messages []object
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		message, e := parseObject([]byte(raw))
		if e != nil {
			rows.Close()
			return e
		}
		message["id"] = id
		messages = append(messages, message)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	firstReadingPropagateConversation(messages)
	for _, message := range messages {
		if message["basisCorrected"] == true && str(message, "role") == "assistant" {
			if _, err = tx.Exec("UPDATE android_fields SET extra=json_set(extra,'$.basisCorrected',json('true')) WHERE user_id=? AND kind='conversation' AND id=?", user, str(message, "id")); err != nil {
				return err
			}
		}
	}
	return nil
}
