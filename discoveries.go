package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const discoverySchema = `
CREATE TABLE IF NOT EXISTS discoveries (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
 version INTEGER NOT NULL, status TEXT NOT NULL, data TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS proposal_basis_refs (
 proposal_id TEXT PRIMARY KEY REFERENCES proposals(id) ON DELETE CASCADE,
 refs TEXT NOT NULL
);`

type GoalBasisRef struct {
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}
type CurrentGoalBasis struct {
	Kind            string   `json:"kind"`
	ID              string   `json:"id"`
	AcceptedVersion int      `json:"acceptedVersion"`
	CurrentVersion  int      `json:"currentVersion,omitempty"`
	Text            string   `json:"text,omitempty"`
	Body            string   `json:"body,omitempty"`
	Status          string   `json:"status"`
	Mode            string   `json:"mode,omitempty"`
	Citations       []object `json:"citations,omitempty"`
}

func basisKind(ref GoalBasisRef) string {
	if ref.Kind == "" {
		return "fact"
	}
	return ref.Kind
}

var discoverySecret = regexp.MustCompile(`(?i)验证码|授权码|\b(?:password|passcode|api[_ -]?key|bearer)\b|verification code`)

func safeDiscoveryText(text string) bool {
	return !retentionSecret.MatchString(text) && !retentionPrivate.MatchString(text) && !discoverySecret.MatchString(text)
}

func discoveryRecord(tx *sql.Tx, user, id string) (object, error) {
	var raw string
	if err := tx.QueryRow("SELECT data FROM discoveries WHERE user_id=? AND id=?", user, id).Scan(&raw); err != nil {
		return nil, missing(err)
	}
	return parseObject([]byte(raw))
}
func currentFactReference(tx *sql.Tx, user, id string) (object, error) {
	memory, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, id))
	if err != nil {
		return nil, missing(err)
	}
	if !memory.Confirmed || !safeDiscoveryText(memory.Text) {
		return nil, ErrConflict
	}
	var history, at string
	if err = tx.QueryRow("SELECT history,COALESCE((SELECT json_extract(extra,'$.at') FROM android_fields WHERE user_id=? AND kind='facts' AND id=?),'') FROM memories WHERE user_id=? AND id=?", user, id, user, id).Scan(&history, &at); err != nil {
		return nil, err
	}
	provenance := "user-confirmation"
	if history != "[]" && history != "null" {
		provenance = "user-correction"
	}
	// Discovery corrections are explicit independent personal statements. Existing
	// source-backed memories retain their association when used as ordinary facts.
	ref := object{"key": "f:" + id, "id": id, "version": memory.Version, "quote": memory.Text, "text": memory.Text, "kind": "self", "provenance": provenance, "sourceBacked": false, "sourceId": nil, "sourceTitle": "", "start": nil, "end": nil, "documentIds": []any{}, "statedAt": ""}
	if firstReadingDate.MatchString(at) {
		ref["statedAt"] = at
	}
	if memory.SourceID != "" {
		return nil, ErrConflict
	}
	return ref, nil
}

// Check exact owned source/fact references. This checks provenance and current
// evidence, not the semantic truth of the adviser's generated interpretation.
func discoveryCitationCurrent(tx *sql.Tx, user string, ref object) error {
	quote := str(ref, "quote")
	version, err := integer(ref["version"])
	if err != nil || version < 1 || quote == "" || !safeDiscoveryText(quote) {
		return ErrConflict
	}
	var cited *Memory
	if id := str(ref, "id"); id != "" {
		memory, history, e := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, id))
		if e != nil {
			return e
		}
		if memory.Version != version || str(ref, "key") != "f:"+id {
			return ErrConflict
		}
		cited = &memory
		if memory.Confirmed || (memory.RetainedReading && memory.SourceID == "") {
			provenance := "retained-reading"
			kind := "retained"
			if memory.Confirmed {
				provenance = "user-confirmation"
				kind = "self"
				if len(history) > 0 {
					provenance = "user-correction"
				}
			}
			if quote != memory.Text || ref["sourceBacked"] != false || str(ref, "provenance") != provenance || str(ref, "kind") != kind || !safeDiscoveryText(memory.Quote) {
				return ErrConflict
			}
			for _, span := range memory.Evidence {
				var body string
				var dismissed bool
				if e = tx.QueryRow("SELECT body FROM sources WHERE user_id=? AND id=?", user, span.SourceID).Scan(&body); e != nil {
					return missing(e)
				}
				if retentionPrivate.MatchString(body) || !safeDiscoveryText(span.Quote) || span.Start < 0 || span.End > len(body) || span.End <= span.Start || body[span.Start:span.End] != span.Quote {
					return ErrConflict
				}
				if e = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM dismissed WHERE user_id=? AND source_id=? AND start_byte<? AND ?<end_byte)", user, span.SourceID, span.End, span.Start).Scan(&dismissed); e != nil {
					return e
				}
				if dismissed {
					return ErrConflict
				}
			}
			return nil
		}
	}
	sourceID := str(ref, "sourceId")
	var src Source
	if err = tx.QueryRow("SELECT id,title,kind,origin,body FROM sources WHERE user_id=? AND id=?", user, sourceID).Scan(&src.ID, &src.Title, &src.Kind, &src.Origin, &src.Text); err != nil {
		return missing(err)
	}
	if (src.Kind != "self" && src.Kind != "file" && src.Kind != "mail") || retentionPrivate.MatchString(src.Text) || ref["sourceBacked"] != true {
		return ErrConflict
	}
	var calendar string
	err = tx.QueryRow("SELECT COALESCE(json_extract(extra,'$.calendarChange.kind'),'') FROM android_fields WHERE user_id=? AND kind='sources' AND id=?", user, sourceID).Scan(&calendar)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if calendar == "cancelled" || calendar == "moved" {
		return ErrConflict
	}
	start, e1 := integer(ref["start"])
	end, e2 := integer(ref["end"])
	if e1 != nil || e2 != nil || end <= start {
		return ErrConflict
	}
	offsets := indexText(src.Text)
	a, e1 := offsets.offset(start, true)
	z, e2 := offsets.offset(end, true)
	if e1 != nil || e2 != nil || a < 0 || z > len(src.Text) || src.Text[a:z] != quote {
		return ErrConflict
	}
	if cited == nil {
		if version != 1 || str(ref, "key") != fmt.Sprintf("r:%s:%d:%d", sourceID, start, end) {
			return ErrConflict
		}
	} else {
		found := false
		for _, span := range cited.Evidence {
			if span.SourceID == sourceID && span.Start == a && span.End == z && span.Quote == quote {
				found = true
			}
		}
		if !found {
			return ErrConflict
		}
	}
	// Build the same scoped exclusions as the browser. A correction to one
	// complete sentence may expose another complete component, never an arbitrary
	// fragment that silently drops a still-current qualification.
	type excluded struct {
		a, z int
		text string
	}
	blocks := []excluded{}
	rows, err := tx.Query("SELECT d.start_byte,d.end_byte FROM dismissed d JOIN sources s ON s.id=d.source_id AND s.user_id=d.user_id WHERE d.user_id=? AND s.body=?", user, src.Text)
	if err != nil {
		return err
	}
	for rows.Next() {
		var x, y int
		if err = rows.Scan(&x, &y); err != nil {
			rows.Close()
			return err
		}
		if x >= 0 && y > x && y <= len(src.Text) {
			blocks = append(blocks, excluded{x, y, firstReadingTrim(firstReadingSpaces.ReplaceAllString(src.Text[x:y], " "))})
		}
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
	var memories []Memory
	for rows.Next() {
		m, _, e := readMemory(rows)
		if e != nil {
			rows.Close()
			return e
		}
		memories = append(memories, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range memories {
		for _, span := range m.Evidence {
			var body string
			e := tx.QueryRow("SELECT body FROM sources WHERE user_id=? AND id=?", user, span.SourceID).Scan(&body)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if body == src.Text && span.Start >= 0 && span.End > span.Start && span.End <= len(body) {
				blocks = append(blocks, excluded{span.Start, span.End, firstReadingTrim(firstReadingSpaces.ReplaceAllString(body[span.Start:span.End], " "))})
			}
		}
	}
	suppressed := map[string]bool{}
	var metadata sql.NullString
	err = tx.QueryRow("SELECT metadata FROM android_dossier WHERE user_id=?", user).Scan(&metadata)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if metadata.Valid {
		o, e := parseObject([]byte(metadata.String))
		if e != nil {
			return e
		}
		items, _ := objects(o["readingSuppressions"])
		for _, item := range items {
			v, _ := integer(item["version"])
			if v == 1 && str(item, "documentHash") == firstReadingHash("document", src.Text) {
				suppressed[str(item, "passageHash")] = true
			}
		}
	}
	blocked := func(span firstReadingPassage) bool {
		q := src.Text[span.start:span.end]
		normal := firstReadingTrim(firstReadingSpaces.ReplaceAllString(q, " "))
		for _, b := range blocks {
			if b.a < span.end && span.start < b.z || b.text != "" && strings.Contains(normal, b.text) {
				return true
			}
		}
		return suppressed[firstReadingHash("passage", q)]
	}
	for _, span := range firstReadingPassages(src) {
		candidates := []firstReadingPassage{span}
		if len(span.components) > 0 && blocked(span) {
			candidates = span.components
		}
		for _, part := range candidates {
			// The browser trims a candidate component before emitting it once
			// its parent is suppressed; sentence components keep their original
			// whitespace offsets here, so trim the same way before comparing.
			trimmed := firstReadingTrim(src.Text[part.start:part.end])
			if trimmed == "" {
				continue
			}
			candidate := part
			candidate.start = part.start + strings.Index(src.Text[part.start:part.end], trimmed)
			candidate.end = candidate.start + len(trimmed)
			if candidate.start == a && candidate.end == z && !blocked(candidate) &&
				str(ref, "kind") == candidate.kind && str(ref, "provenance") == candidate.kind+"-quote" {
				return nil
			}
		}
	}
	return ErrConflict
}

func resolveDiscoveryTx(tx *sql.Tx, user string, row object) (object, error) {
	out := maps.Clone(row)
	if str(row, "status") == "stale" {
		out["title"], out["body"], out["citations"] = "", "", []any{}
		return out, nil
	}
	refs, err := objects(row["citations"])
	if err != nil {
		return nil, err
	}
	if id := str(row, "correctionId"); id != "" {
		ref, e := currentFactReference(tx, user, id)
		if e == nil {
			version, _ := integer(row["version"])
			prior := 0
			for _, old := range refs {
				if str(old, "id") == id {
					prior, _ = integer(old["version"])
				}
			}
			current, _ := integer(ref["version"])
			if prior > 0 && current > prior {
				version += current - prior
			}
			out["version"], out["title"], out["body"], out["citations"], out["status"] = version, ref["quote"], "", []any{ref}, "corrected"
			return out, nil
		}
		if !errors.Is(e, ErrConflict) && !errors.Is(e, ErrNotFound) {
			return nil, e
		}
	} else {
		valid := len(refs) > 0
		for _, ref := range refs {
			e := discoveryCitationCurrent(tx, user, ref)
			if e != nil {
				if !errors.Is(e, ErrConflict) && !errors.Is(e, ErrNotFound) {
					return nil, e
				}
				valid = false
			}
		}
		if valid {
			return out, nil
		}
	}
	version, _ := integer(row["version"])
	out["version"], out["status"], out["title"], out["body"], out["citations"] = version+1, "stale", "", "", []any{}
	return out, nil
}
func saveDiscoveryTx(tx *sql.Tx, user string, row object) error {
	id := str(row, "id")
	version, err := integer(row["version"])
	status := str(row, "status")
	if id == "" || err != nil || version < 1 || (status != "proposed" && status != "corrected" && status != "stale") {
		return ErrInvalid
	}
	if status != "stale" && (str(row, "title") == "" || !safeDiscoveryText(str(row, "title")) || utf8.RuneCountInString(str(row, "title")) > 1000) {
		return ErrInvalid
	}
	if !safeDiscoveryText(str(row, "body")) || utf8.RuneCountInString(str(row, "body")) > 900 {
		return ErrInvalid
	}
	if status == "proposed" && (str(row, "correctionId") != "" || (str(row, "kind") != "finding" && str(row, "kind") != "question")) {
		return ErrInvalid
	}
	// Corrected means an explicit personal statement replaced the interpretation;
	// raw dossier input cannot fabricate that label without its fact reference.
	if status == "corrected" && str(row, "correctionId") == "" {
		return ErrInvalid
	}
	current, err := resolveDiscoveryTx(tx, user, row)
	if err != nil {
		return err
	}
	if status != "stale" && str(current, "status") == "stale" {
		return ErrConflict
	}
	result, err := tx.Exec(`INSERT INTO discoveries(id,user_id,version,status,data) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version,status=excluded.status,data=excluded.data WHERE user_id=excluded.user_id`, id, user, current["version"], current["status"], encode(current))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrInvalid
	}
	return err
}
func refreshDiscoveries(tx *sql.Tx, user string) error {
	rows, err := tx.Query("SELECT data FROM discoveries WHERE user_id=?", user)
	if err != nil {
		return err
	}
	var records []object
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		o, e := parseObject([]byte(raw))
		if e != nil {
			rows.Close()
			return e
		}
		records = append(records, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, row := range records {
		current, e := resolveDiscoveryTx(tx, user, row)
		if e != nil {
			return e
		}
		if encode(current) != encode(row) {
			if _, e = tx.Exec("UPDATE discoveries SET version=?,status=?,data=? WHERE user_id=? AND id=?", current["version"], current["status"], encode(current), user, str(row, "id")); e != nil {
				return e
			}
		}
	}
	return nil
}
func resolveGoalBasisTx(tx *sql.Tx, user string, ref GoalBasisRef) (CurrentGoalBasis, error) {
	kind := basisKind(ref)
	out := CurrentGoalBasis{Kind: kind, ID: ref.ID, AcceptedVersion: ref.Version, Status: "stale"}
	if kind == "fact" {
		m, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, ref.ID))
		if errors.Is(err, ErrNotFound) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.CurrentVersion, out.Text, out.Status = m.Version, m.Text, "current"
		return out, nil
	}
	if kind != "discovery" {
		return out, ErrInvalid
	}
	row, err := discoveryRecord(tx, user, ref.ID)
	if errors.Is(err, ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	row, err = resolveDiscoveryTx(tx, user, row)
	if err != nil {
		return out, err
	}
	out.CurrentVersion, _ = integer(row["version"])
	out.Status = str(row, "status")
	out.Mode = out.Status
	out.Text, out.Body = str(row, "title"), str(row, "body")
	out.Citations, _ = objects(row["citations"])
	return out, nil
}
func validateGoalRefsTx(tx *sql.Tx, user string, refs []GoalBasisRef) error {
	seen := map[string]bool{}
	for _, ref := range refs {
		key := basisKind(ref) + ":" + ref.ID
		if ref.ID == "" || ref.Version < 1 || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		current, err := resolveGoalBasisTx(tx, user, ref)
		if err != nil {
			return err
		}
		if current.Status == "stale" || current.CurrentVersion != ref.Version {
			return ErrConflict
		}
	}
	return nil
}
func (s *Store) Discoveries(user string) ([]object, error) {
	out := []object{}
	err := s.write(func(tx *sql.Tx) error {
		if err := refreshDiscoveries(tx, user); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT data FROM discoveries WHERE user_id=? ORDER BY rowid", user)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			row, e := parseObject([]byte(raw))
			if e != nil {
				return e
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}
func (s *Store) CreateDiscovery(user, title, body, kind string, refs []object) (object, error) {
	row := object{"id": opaqueID(18), "version": 1, "status": "proposed", "title": strings.TrimSpace(title), "body": strings.TrimSpace(body), "kind": kind, "citations": refs, "at": time.Now().UTC().Format(time.RFC3339)}
	row, _ = parseObject([]byte(encode(row)))
	err := s.write(func(tx *sql.Tx) error { return saveDiscoveryTx(tx, user, row) })
	return row, err
}
func refsFromValue(value any) ([]GoalBasisRef, error) {
	var refs []GoalBasisRef
	err := json.Unmarshal([]byte(encode(value)), &refs)
	return refs, err
}

func saveProposalRefsTx(tx *sql.Tx, id string, value any) error {
	refs, err := refsFromValue(value)
	if err != nil {
		return ErrInvalid
	}
	for _, ref := range refs {
		if ref.ID == "" || ref.Version < 1 || (basisKind(ref) != "fact" && basisKind(ref) != "discovery") {
			return ErrInvalid
		}
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO proposal_basis_refs(proposal_id,refs) VALUES(?,?)", id, encode(value))
	return err
}
func proposalRefsTx(tx *sql.Tx, user, id string) ([]GoalBasisRef, error) {
	var raw string
	err := tx.QueryRow("SELECT COALESCE(r.refs,'[]') FROM proposals p LEFT JOIN proposal_basis_refs r ON r.proposal_id=p.id WHERE p.user_id=? AND p.id=?", user, id).Scan(&raw)
	if err != nil {
		return nil, missing(err)
	}
	var refs []GoalBasisRef
	err = json.Unmarshal([]byte(raw), &refs)
	return refs, err
}
func (s *Store) ProposeWithRefs(user, title string, refs []GoalBasisRef) (Proposal, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 160 {
		return Proposal{}, ErrInvalid
	}
	p := Proposal{ID: opaqueID(18), Title: title, At: time.Now().UTC().Format(time.RFC3339), BasisRefs: refs}
	err := s.write(func(tx *sql.Tx) error {
		if err := validateGoalRefsTx(tx, user, refs); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO proposals(id,user_id,title,created_at) VALUES(?,?,?,?)", p.ID, user, p.Title, p.At); err != nil {
			return err
		}
		return saveProposalRefsTx(tx, p.ID, refs)
	})
	return p, err
}
func (s *Store) AcceptProposalWithRefs(user, id, title, step, unit string, refs []GoalBasisRef, methods []GoalMethod) (Goal, error) {
	title, step = strings.TrimSpace(title), strings.TrimSpace(step)
	if title == "" || utf8.RuneCountInString(title) > 160 || step == "" || utf8.RuneCountInString(step) > 300 || !validUnit(unit) {
		return Goal{}, ErrInvalid
	}
	g := Goal{ID: opaqueID(18), ProposalID: id, Title: title, Step: step, Unit: unit, Entries: []GoalEntry{}, At: time.Now().UTC().Format(time.RFC3339), ApprovedBasis: refs, ApprovedMethods: methods}
	err := s.write(func(tx *sql.Tx) error {
		offered, err := proposalRefsTx(tx, user, id)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			found := false
			for _, candidate := range offered {
				if basisKind(ref) == basisKind(candidate) && ref.ID == candidate.ID {
					found = true
				}
			}
			if !found {
				return ErrConflict
			}
		}
		if err = validateGoalRefsTx(tx, user, refs); err != nil {
			return err
		}
		for _, m := range methods {
			if strings.TrimSpace(m.Text) == "" || utf8.RuneCountInString(m.Text) > 300 {
				return ErrInvalid
			}
		}
		if _, err = tx.Exec("INSERT INTO goals(id,user_id,title,step,unit,created_at) VALUES(?,?,?,?,?,?)", g.ID, user, g.Title, g.Step, g.Unit, g.At); err != nil {
			return err
		}
		row, _ := parseObject([]byte(encode(g)))
		if err = saveGoalChoices(tx, row); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM proposals WHERE user_id=? AND id=?", user, id); err != nil {
			return err
		}
		for _, ref := range refs {
			current, e := resolveGoalBasisTx(tx, user, ref)
			if e != nil {
				return e
			}
			g.CurrentBasis = append(g.CurrentBasis, current)
		}
		return nil
	})
	return g, err
}
func (s *Store) CorrectDiscovery(user, id, text string, version int) (object, error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 1000 || !safeDiscoveryText(text) {
		return nil, ErrInvalid
	}
	var out object
	err := s.write(func(tx *sql.Tx) error {
		row, err := discoveryRecord(tx, user, id)
		if err != nil {
			return err
		}
		row, err = resolveDiscoveryTx(tx, user, row)
		if err != nil {
			return err
		}
		current, _ := integer(row["version"])
		if current != version || str(row, "status") == "stale" {
			return ErrConflict
		}
		factID := str(row, "correctionId")
		if factID == "" {
			factID = opaqueID(18)
			if _, err = tx.Exec("INSERT INTO memories(id,user_id,current_text,quote,start_byte,end_byte,category,attribution,confirmed,version) VALUES(?,?,?,'',0,0,?,'confirmed',1,1)", factID, user, text, inferCategory(text, "self")); err != nil {
				return err
			}
		} else {
			var previous string
			if err = tx.QueryRow("SELECT current_text FROM memories WHERE user_id=? AND id=? AND confirmed=1", user, factID).Scan(&previous); err != nil {
				return missing(err)
			}
			if err = androidChangedFact(tx, user, factID, false); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE memories SET current_text=?,version=version+1,history=json_insert(history,'$[#]',json_object('text',?,'at',?)) WHERE user_id=? AND id=?", text, previous, time.Now().UTC().Format(time.RFC3339), user, factID); err != nil {
				return err
			}
		}
		ref, err := currentFactReference(tx, user, factID)
		if err != nil {
			return err
		}
		row["version"], row["status"], row["title"], row["body"], row["kind"], row["correctionId"], row["citations"] = current+1, "corrected", text, "", "finding", factID, []any{ref}
		if err = saveDiscoveryTx(tx, user, row); err != nil {
			return err
		}
		if err = syncAndroidDiscovery(tx, user, row); err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

func syncAndroidDiscovery(tx *sql.Tx, user string, row object) error {
	var raw sql.NullString
	err := tx.QueryRow("SELECT metadata FROM android_dossier WHERE user_id=?", user).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || !raw.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	metadata, err := parseObject([]byte(raw.String))
	if err != nil {
		return err
	}
	reveal, _ := metadata["reveal"].(object)
	task, _ := reveal["firstReading"].(object)
	if str(task, "discoveryId") != str(row, "id") {
		return nil
	}
	revision, _ := integer(task["revision"])
	task["revision"] = revision + 1
	task["status"] = "corrected"
	task["correctionId"] = row["correctionId"]
	task["selection"] = row["citations"]
	task["result"] = object{"title": row["title"], "body": "", "kind": "finding", "citations": row["citations"]}
	if _, err = tx.Exec("UPDATE android_dossier SET metadata=? WHERE user_id=?", encode(metadata), user); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE android_fields SET extra=json_set(extra,'$.basisCorrected',json('true')) WHERE user_id=? AND kind='conversation' AND json_extract(extra,'$.role')='assistant' AND (json_extract(extra,'$.firstReadingId')=? OR EXISTS(SELECT 1 FROM json_each(extra,'$.discoveryReferences') WHERE json_extract(value,'$.id')=?))`, user, str(task, "id"), str(task, "id")); err != nil {
		return err
	}
	return androidPropagateDiscoveryConversation(tx, user)
}
