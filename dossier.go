package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Android has one private owner. No bearer credential or TCP listener is needed.
const dossierSchema = `
CREATE TABLE IF NOT EXISTS android_dossier (
	slot INTEGER PRIMARY KEY CHECK(slot=1), user_id TEXT NOT NULL UNIQUE REFERENCES users(id),
	revision INTEGER NOT NULL DEFAULT 0, metadata TEXT, signature BLOB, last_input BLOB
);
CREATE TABLE IF NOT EXISTS android_fields (
	user_id TEXT NOT NULL REFERENCES users(id), kind TEXT NOT NULL, id TEXT NOT NULL,
	position INTEGER NOT NULL, extra TEXT NOT NULL, PRIMARY KEY(user_id,kind,id)
);
CREATE TABLE IF NOT EXISTS goal_choices (
	goal_id TEXT PRIMARY KEY REFERENCES goals(id) ON DELETE CASCADE,
	first_extra TEXT NOT NULL, further_basis TEXT NOT NULL, methods TEXT NOT NULL
);`

type object = map[string]any

func parseObject(raw []byte) (object, error) {
	var o object
	if !json.Valid(raw) {
		return nil, ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil || o == nil {
		return nil, ErrInvalid
	}
	return o, nil
}

func encode(v any) string {
	// All callers pass values decoded from valid JSON or scalar database fields.
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func str(o object, k string) string { s, _ := o[k].(string); return s }
func integer(v any) (int, error) {
	n, err := strconv.Atoi(fmt.Sprint(v))
	if err != nil || n < 0 {
		return 0, ErrInvalid
	}
	return n, nil
}

func objects(v any) ([]object, error) {
	if v == nil {
		return []object{}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, ErrInvalid
	}
	out := make([]object, 0, len(a))
	for _, item := range a {
		o, ok := item.(object)
		if !ok || o == nil {
			return nil, ErrInvalid
		}
		out = append(out, o)
	}
	return out, nil
}

// Only fields not represented in the original Store tables live in android_fields.
// Chat turns have no previous Go representation and are stored there individually.
type dossierTable struct {
	kind, table, projection, keys string
	desc                          bool
}

var dossierTables = []dossierTable{
	{"sources", "sources", `json_object('id',t.id,'kind',t.kind,'origin',t.origin,'title',t.title,'text',t.body,'at',t.created_at)`, "id kind origin title text at", false},
	{"facts", "memories", `json_object('id',t.id,'sourceId',t.source_id,'text',t.current_text,'quote',t.quote,'start',t.start_byte,'end',t.end_byte,'category',t.category,'attribution',t.attribution,'confirmed',json(iif(t.confirmed,'true','false')),'version',t.version,'history',json(t.history),'alternatives',json(t.alternatives),'evidence',json(t.evidence))`, "id sourceId text quote start end category attribution confirmed version history alternatives evidence", true},
	{"discoveries", "discoveries", `json(t.data)`, "id version status title body kind citations correctionId taskId attemptId at", false},
	{"proposals", "proposals", `json_object('id',t.id,'title',t.title,'basis',t.basis_id,'at',t.created_at)`, "id title basis at basisRefs", true},
	{"goals", "goals", `json_object('id',t.id,'title',t.title,'step',t.step,'unit',t.unit,'at',t.created_at)`, "id title step unit at entries proposalId approvedBasis approvedMethods", true},
	{"entries", "goal_entries", `json_object('id',t.id,'amount',t.amount,'note',t.note,'at',t.created_at)`, "id amount note at", true},
	{"reports", "reports", `json_object('id',t.id,'at',t.created_at,'from',t.from_at,'rows',json(t.rows_json))`, "id at from rows", true},
}

type dossierRows struct {
	fields              object
	rows                map[string][]object
	sources             map[string]object
	indexes             spanIndexes
	entries             map[string]string // entry ID -> goal ID
	discoveriesProvided bool
}

func splitDossier(o object) (*dossierRows, error) {
	version, err := integer(o["version"])
	if err != nil || (version != 1 && version != 2) {
		return nil, ErrInvalid
	}
	d := &dossierRows{fields: maps.Clone(o), rows: map[string][]object{}, sources: map[string]object{}, indexes: spanIndexes{}, entries: map[string]string{}}
	_, d.discoveriesProvided = o["discoveries"]
	for _, key := range []string{"sources", "facts", "discoveries", "proposals", "goals", "reports", "dismissed", "conversation"} {
		items, err := objects(o[key])
		if err != nil {
			return nil, err
		}
		d.rows[key] = items
		delete(d.fields, key)
	}
	for _, s := range d.rows["sources"] {
		d.sources[str(s, "id")] = s
	}
	// 自动提取的事实以用户刚发的那条对话为出处。对话不是导入资料，
	// 不写入 sources 表，只在本次校验里提供原文，避免复制整段聊天。
	for _, m := range d.rows["conversation"] {
		if str(m, "role") != "user" || m["ephemeral"] == true {
			continue
		}
		id, text := str(m, "id"), str(m, "text")
		if id == "" || text == "" {
			continue
		}
		d.sources["chat:"+id] = object{"id": "chat:" + id, "kind": "self", "origin": "conversation", "title": "对话记录", "text": text}
	}
	for _, g := range d.rows["goals"] {
		entries, err := objects(g["entries"])
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			d.entries[str(e, "id")] = str(g, "id")
		}
		d.rows["entries"] = append(d.rows["entries"], entries...)
	}
	for _, group := range []string{"sources", "facts", "discoveries", "proposals", "goals", "entries", "reports", "conversation"} {
		seen := map[string]bool{}
		for _, r := range d.rows[group] {
			id := str(r, "id")
			if id == "" || seen[id] {
				return nil, ErrInvalid
			}
			seen[id] = true
		}
	}
	normalizeModelPreparations(d)
	if err := validateModelPreparations(d); err != nil {
		return nil, err
	}
	normalizeGoalReviews(d)
	if err := validateGoalReviews(d); err != nil {
		return nil, err
	}
	return d, nil
}

// JS string positions count UTF-16 code units. Go and SQLite count UTF-8 bytes.
// Reject a position inside a surrogate pair or multi-byte UTF-8 sequence.
type textPosition struct{ bytes, units int }
type textOffsets struct {
	body   string
	points []textPosition
}

// Remember sparse character boundaries once per source. Evidence can arrive in
// any order (including several overlapping quotations of the same passage).
func indexText(body string) textOffsets {
	var points []textPosition
	units, next := 0, 256
	for i, r := range body {
		if i >= next {
			points = append(points, textPosition{i, units})
			next = i + 256
		}
		units++
		if r > 0xffff {
			units++
		}
	}
	return textOffsets{body, points}
}

func (idx textOffsets) offset(position int, toBytes bool) (int, error) {
	n := sort.Search(len(idx.points), func(i int) bool {
		if toBytes {
			return idx.points[i].units > position
		}
		return idx.points[i].bytes > position
	})
	var p textPosition
	if n > 0 {
		p = idx.points[n-1]
	}
	for {
		if toBytes && p.units == position {
			return p.bytes, nil
		}
		if !toBytes && p.bytes == position {
			return p.units, nil
		}
		if p.bytes == len(idx.body) || (toBytes && p.units > position) || (!toBytes && p.bytes > position) {
			return 0, ErrInvalid
		}
		r, width := utf8.DecodeRuneInString(idx.body[p.bytes:])
		p.bytes += width
		p.units++
		if r > 0xffff {
			p.units++
		}
	}
}

type spanIndexes map[string]textOffsets

func (indexes spanIndexes) forSource(id, body string) textOffsets {
	idx, ok := indexes[id]
	if !ok {
		idx = indexText(body)
		indexes[id] = idx
	}
	return idx
}

func (indexes spanIndexes) convertSpan(o object, sources map[string]object, toBytes, checkQuote bool) error {
	s, ok := sources[str(o, "sourceId")]
	if !ok {
		return ErrInvalid
	}
	body := str(s, "text")
	start, err := integer(o["start"])
	if err != nil {
		return err
	}
	end, err := integer(o["end"])
	if err != nil || end <= start {
		return ErrInvalid
	}
	idx := indexes.forSource(str(o, "sourceId"), body)
	newStart, err := idx.offset(start, toBytes)
	if err != nil {
		return err
	}
	newEnd, err := idx.offset(end, toBytes)
	if err != nil {
		return err
	}
	i, j := start, end
	if toBytes {
		i, j = newStart, newEnd
	}
	if i < 0 || j <= i || j > len(body) {
		return ErrInvalid
	}
	if checkQuote && body[i:j] != str(o, "quote") {
		return ErrInvalid
	}
	o["start"], o["end"] = newStart, newEnd
	return nil
}

func decodeArray(raw string) ([]any, error) {
	var a []any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		return nil, err
	}
	if a == nil {
		a = []any{}
	}
	return a, nil
}

func loadDossier(tx *sql.Tx, user string, metadata sql.NullString) (object, error) {
	o := object{"version": 2}
	if metadata.Valid {
		var err error
		o, err = parseObject([]byte(metadata.String))
		if err != nil {
			return nil, err
		}
	}
	sources, goals := map[string]object{}, map[string]object{}
	indexes := spanIndexes{}
	hasData := metadata.Valid
	// 对话出处只在读取时临时加入，不写回 sources 表。必须在读取
	// memories 之前准备好，否则自动提取的事实找不到原句。
	chat, conversation, err := chatSources(tx, user)
	if err != nil {
		return nil, err
	}
	for id, source := range chat {
		sources[id] = source
	}
	for _, t := range dossierTables {
		q := "SELECT " + t.projection + ",COALESCE(v.extra,'{}')"
		if t.kind == "entries" {
			q += ",t.goal_id"
		}
		q += " FROM " + t.table + " t LEFT JOIN android_fields v ON v.user_id=t.user_id AND v.kind=? AND v.id=t.id WHERE t.user_id=? ORDER BY (v.position IS NULL)"
		if t.desc {
			q += " DESC"
		}
		q += ",v.position,t.rowid"
		if t.desc {
			q += " DESC"
		}
		rows, err := tx.Query(q, t.kind, user)
		if err != nil {
			return nil, err
		}
		items := []any{}
		for rows.Next() {
			var raw, extra, goalID string
			args := []any{&raw, &extra}
			if t.kind == "entries" {
				args = append(args, &goalID)
			}
			if err = rows.Scan(args...); err != nil {
				break
			}
			r, e := parseObject([]byte(raw))
			if e != nil {
				err = e
				break
			}
			fields, e := parseObject([]byte(extra))
			if e != nil {
				err = e
				break
			}
			maps.Copy(r, fields)
			id := str(r, "id")
			switch t.kind {
			case "sources":
				sources[id] = r
			case "facts":
				if str(r, "sourceId") == "" {
					delete(r, "sourceId")
					delete(r, "start")
					delete(r, "end")
					if _, ok := r["source"]; !ok {
						if r["confirmed"] == true {
							r["source"] = "本人确认"
						} else {
							r["source"] = "已断开的资料"
							r["retainedReading"] = true
						}
					}
				} else {
					r["source"] = str(sources[str(r, "sourceId")], "title")
					if err = indexes.convertSpan(r, sources, false, false); err != nil {
						break
					}
				}
				spans, e := objects(r["evidence"])
				if e != nil {
					err = e
					break
				}
				for _, sp := range spans {
					if err = indexes.convertSpan(sp, sources, false, true); err != nil {
						break
					}
				}
				history, _ := r["history"].([]any)
				for i, h := range history {
					if s, ok := h.(string); ok {
						history[i] = object{"text": s}
					}
				}
				if history == nil {
					r["history"] = []any{}
				}
			case "goals":
				r["entries"] = []any{}
				goals[id] = r
			case "entries":
				if g := goals[goalID]; g != nil {
					g["entries"] = append(g["entries"].([]any), r)
				}
			}
			if err != nil {
				break
			}
			items = append(items, r)
			hasData = true
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return nil, err
		}
		if t.kind != "entries" && (t.kind != "discoveries" || len(items) > 0) {
			o[t.kind] = items
		}
	}
	proposals, _ := objects(o["proposals"])
	for _, proposal := range proposals {
		var raw string
		e := tx.QueryRow("SELECT refs FROM proposal_basis_refs WHERE proposal_id=?", str(proposal, "id")).Scan(&raw)
		if e == nil {
			var refs any
			if e = json.Unmarshal([]byte(raw), &refs); e != nil {
				return nil, e
			}
			proposal["basisRefs"] = refs
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	rows, err := tx.Query(`SELECT a.goal_id,a.proposal_id,a.memory_id,a.accepted_version,COALESCE(c.first_extra,'{}'),COALESCE(c.further_basis,'[]'),COALESCE(c.methods,'[]')
		FROM goal_acceptances a JOIN goals g ON g.id=a.goal_id LEFT JOIN goal_choices c ON c.goal_id=a.goal_id WHERE g.user_id=?`, user)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var goal, proposal, first, firstExtra, basis, methods string
		var ver int
		if err = rows.Scan(&goal, &proposal, &first, &ver, &firstExtra, &basis, &methods); err != nil {
			break
		}
		b, e := decodeArray(basis)
		if e != nil {
			err = e
			break
		}
		if first != "" {
			firstRef, e := parseObject([]byte(firstExtra))
			if e != nil {
				err = e
				break
			}
			firstRef["id"], firstRef["version"] = first, ver
			b = append([]any{firstRef}, b...)
		}
		m, e := decodeArray(methods)
		if e != nil {
			err = e
			break
		}
		g := goals[goal]
		g["proposalId"], g["approvedBasis"], g["approvedMethods"] = proposal, b, m
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(`SELECT d.source_id,d.start_byte,d.end_byte,COALESCE(v.extra,'{}') FROM dismissed d
		LEFT JOIN android_fields v ON v.user_id=d.user_id AND v.kind='dismissed' AND v.id=json_array(d.source_id,d.start_byte,d.end_byte)
		WHERE d.user_id=? ORDER BY (v.position IS NULL),v.position,d.rowid`, user)
	if err != nil {
		return nil, err
	}
	o["dismissed"] = []any{}
	for rows.Next() {
		var id, raw string
		var start, end int
		if err = rows.Scan(&id, &start, &end, &raw); err != nil {
			break
		}
		r, e := parseObject([]byte(raw))
		if e != nil {
			err = e
			break
		}
		r["sourceId"], r["start"], r["end"] = id, start, end
		if err = indexes.convertSpan(r, sources, false, false); err != nil {
			break
		}
		o["dismissed"] = append(o["dismissed"].([]any), r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	o["conversation"] = conversation
	if !hasData {
		return nil, nil
	}
	return o, nil
}

func (d *dossierRows) saveRecord(tx *sql.Tx, user, kind string, r object) error {
	var table, names string
	var args []any
	switch kind {
	case "sources":
		table, names = "sources", "kind origin title body digest created_at"
		body := str(r, "text")
		digest := sha256.Sum256([]byte(body))
		k := str(r, "kind")
		if k == "" {
			k = "self"
		}
		if body == "" || str(r, "title") == "" {
			return ErrInvalid
		}
		args = []any{k, str(r, "origin"), str(r, "title"), body, digest[:], str(r, "at")}
	case "facts":
		table, names = "memories", "source_id current_text quote start_byte end_byte category attribution confirmed version history alternatives evidence"
		id := str(r, "sourceId")
		var source any
		var start, end any = 0, 0
		spans, err := objects(r["evidence"])
		if err != nil {
			return err
		}
		if id != "" {
			source = id
			if _, ok := d.sources[id]; !ok {
				return ErrInvalid
			}
			primary := maps.Clone(r)
			if primary["start"] == nil || primary["end"] == nil {
				quote, body := str(r, "quote"), str(d.sources[id], "text")
				at := strings.Index(body, quote)
				if at < 0 || quote == "" {
					return ErrInvalid
				}
				idx := d.indexes.forSource(id, body)
				primary["start"], _ = idx.offset(at, false)
				primary["end"], _ = idx.offset(at+len(quote), false)
			}
			if r["evidence"] == nil {
				spans = []object{{"sourceId": id, "start": primary["start"], "end": primary["end"], "quote": r["quote"]}}
			}
			if err = d.indexes.convertSpan(primary, d.sources, true, false); err != nil {
				return err
			}
			start, end = primary["start"], primary["end"]
		}
		stored := make([]object, 0, len(spans))
		for _, sp := range spans {
			copy := maps.Clone(sp)
			if err = d.indexes.convertSpan(copy, d.sources, true, true); err != nil {
				return err
			}
			stored = append(stored, copy)
		}
		text := str(r, "text")
		if text == "" {
			return ErrInvalid
		}
		ver := 1
		if r["version"] != nil {
			ver, err = integer(r["version"])
			if err != nil || ver < 1 {
				return ErrInvalid
			}
		}
		history, err := objects(r["history"])
		if err != nil {
			return err
		}
		confirmed, ok := r["confirmed"].(bool)
		if !ok {
			confirmed = len(history) > 0
		}
		k := str(d.sources[id], "kind")
		category, attribution := str(r, "category"), str(r, "attribution")
		if category == "" {
			category = inferCategory(text, k)
		}
		if attribution == "" {
			attribution = inferAttribution(k, confirmed)
		}
		alts, err := objects(r["alternatives"])
		if err != nil {
			return err
		}
		args = []any{source, text, str(r, "quote"), start, end, category, attribution, confirmed, ver, encode(history), encode(alts), encode(stored)}
	case "discoveries":
		return saveDiscoveryTx(tx, user, r)
	case "proposals":
		table, names = "proposals", "title basis_id created_at"
		args = []any{str(r, "title"), r["basis"], str(r, "at")}
	case "goals":
		table, names = "goals", "title step unit created_at"
		args = []any{str(r, "title"), str(r, "step"), str(r, "unit"), str(r, "at")}
	case "entries":
		table, names = "goal_entries", "goal_id amount note created_at"
		n, err := strconv.ParseFloat(fmt.Sprint(r["amount"]), 64)
		if err != nil || !validGoalAmount(n) {
			return ErrInvalid
		}
		args = []any{d.entries[str(r, "id")], n, str(r, "note"), str(r, "at")}
	case "reports":
		table, names = "reports", "from_at created_at rows_json"
		rows, err := objects(r["rows"])
		if err != nil {
			return err
		}
		args = []any{str(r, "from"), str(r, "at"), encode(rows)}
	}
	columns := strings.Fields(names)
	updates := make([]string, len(columns))
	for i, c := range columns {
		updates[i] = c + "=excluded." + c
	}
	query := "INSERT INTO " + table + "(id,user_id," + strings.Join(columns, ",") + ") VALUES(?" + strings.Repeat(",?", len(columns)+1) + ") ON CONFLICT(id) DO UPDATE SET " + strings.Join(updates, ",") + " WHERE user_id=excluded.user_id"
	res, err := tx.Exec(query, append([]any{str(r, "id"), user}, args...)...)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrInvalid
	}
	if kind == "proposals" {
		if refs, ok := r["basisRefs"]; ok {
			return saveProposalRefsTx(tx, str(r, "id"), refs)
		}
	}
	if kind != "goals" {
		return nil
	}
	return saveGoalChoices(tx, r)
}

func saveGoalChoices(tx *sql.Tx, g object) error {
	goal, proposal := str(g, "id"), str(g, "proposalId")
	if proposal == "" {
		_, err := tx.Exec("DELETE FROM goal_acceptances WHERE goal_id=?", goal)
		if err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM goal_choices WHERE goal_id=?", goal)
		return err
	}
	basis, err := objects(g["approvedBasis"])
	if err != nil {
		return err
	}
	methods, err := objects(g["approvedMethods"])
	if err != nil {
		return err
	}
	first, ver := "", 0
	for i, b := range basis {
		v, err := integer(b["version"])
		if err != nil || v == 0 || str(b, "id") == "" {
			return ErrInvalid
		}
		if str(b, "kind") != "" && str(b, "kind") != "fact" && str(b, "kind") != "discovery" {
			return ErrInvalid
		}
		if i == 0 && str(b, "kind") != "discovery" {
			first, ver = str(b, "id"), v
		}
	}
	firstExtra := object{}
	if len(basis) > 0 && first != "" {
		firstExtra = maps.Clone(basis[0])
		delete(firstExtra, "id")
		delete(firstExtra, "version")
		basis = basis[1:]
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO goal_acceptances(goal_id,proposal_id,memory_id,accepted_version) VALUES(?,?,?,?)", goal, proposal, first, ver)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO goal_choices(goal_id,first_extra,further_basis,methods) VALUES(?,?,?,?)", goal, encode(firstExtra), encode(basis), encode(methods))
	return err
}

func saveFields(tx *sql.Tx, user, kind, id string, pos int, extra object) error {
	_, err := tx.Exec(`INSERT INTO android_fields(user_id,kind,id,position,extra) VALUES(?,?,?,?,?)
		ON CONFLICT(user_id,kind,id) DO UPDATE SET position=excluded.position,extra=excluded.extra
		WHERE position!=excluded.position OR extra!=excluded.extra`, user, kind, id, pos, encode(extra))
	return err
}

func saveDossier(tx *sql.Tx, user string, prior object, d *dossierRows) error {
	if prior == nil {
		prior = object{"version": 2}
	}
	old, err := splitDossier(prior)
	if err != nil {
		return err
	}
	if !d.discoveriesProvided {
		d.rows["discoveries"] = old.rows["discoveries"]
	}
	pruneDossierDiscoveryReferences(d)
	type removal struct{ kind, table, id string }
	var remove []removal
	for _, t := range append(slices.Clone(dossierTables), dossierTable{kind: "conversation", keys: "id"}) {
		previous := map[string]object{}
		for _, r := range old.rows[t.kind] {
			previous[str(r, "id")] = r
		}
		for i, r := range d.rows[t.kind] {
			id := str(r, "id")
			prev := previous[id]
			if t.kind == "goals" && (prev == nil || encode(prev["approvedBasis"]) != encode(r["approvedBasis"])) {
				refs, e := refsFromValue(r["approvedBasis"])
				if e != nil {
					return ErrInvalid
				}
				// A restored dossier replays an archive: its goal basis may
				// legitimately be stale by now. Require that a typed basis
				// names a discovery that exists in the imported state, not
				// that it is still current; new acceptance keeps its own
				// currency check at the accept endpoint.
				for _, ref := range refs {
					if basisKind(ref) == "discovery" {
						if ref.ID == "" || ref.Version < 1 {
							return ErrInvalid
						}
						if _, e = discoveryRecord(tx, user, ref.ID); e != nil {
							return e
						}
					}
				}
			}
			delete(previous, id)
			if t.table != "" && (prev == nil || encode(prev) != encode(r) || (t.kind == "entries" && d.entries[id] != old.entries[id])) {
				if err = d.saveRecord(tx, user, t.kind, r); err != nil {
					return err
				}
			}
			fields := maps.Clone(r)
			for _, k := range strings.Fields(t.keys) {
				delete(fields, k)
			}
			if t.kind == "facts" && str(r, "sourceId") != "" {
				delete(fields, "source")
			}
			if err = saveFields(tx, user, t.kind, id, i, fields); err != nil {
				return err
			}
		}
		for id := range previous {
			remove = append(remove, removal{t.kind, t.table, id})
		}
	}
	for _, r := range slices.Backward(remove) {
		if r.table != "" {
			if _, err = tx.Exec("DELETE FROM "+r.table+" WHERE user_id=? AND id=?", user, r.id); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("DELETE FROM android_fields WHERE user_id=? AND kind=? AND id=?", user, r.kind, r.id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM dismissed WHERE user_id=?", user); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM android_fields WHERE user_id=? AND kind='dismissed'", user); err != nil {
		return err
	}
	for i, r := range d.rows["dismissed"] {
		sp := maps.Clone(r)
		if err = d.indexes.convertSpan(sp, d.sources, true, false); err != nil {
			return err
		}
		id, start, end := str(sp, "sourceId"), sp["start"], sp["end"]
		// SQLite constructs the composite key using the exact same JSON encoding as the read join.
		var key string
		if err = tx.QueryRow("SELECT json_array(?,?,?)", id, start, end).Scan(&key); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO dismissed VALUES(?,?,?,?)", user, id, start, end); err != nil {
			return err
		}
		delete(sp, "sourceId")
		delete(sp, "start")
		delete(sp, "end")
		if err = saveFields(tx, user, "dismissed", key, i, sp); err != nil {
			return err
		}
	}
	if err = refreshDiscoveries(tx, user); err != nil {
		return err
	}
	d.fields["version"] = 2
	_, err = tx.Exec("UPDATE android_dossier SET metadata=? WHERE user_id=?", encode(d.fields), user)
	return err
}

// chatSources returns the user's non-ephemeral messages as temporary sources
// for facts extracted from the conversation. They are never inserted into the
// sources table; this is only enough text to validate the quoted span.
func chatSources(tx *sql.Tx, user string) (map[string]object, []any, error) {
	rows, err := tx.Query("SELECT id,extra FROM android_fields WHERE user_id=? AND kind='conversation' ORDER BY position", user)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]object{}
	items := []any{}
	for rows.Next() {
		var id, extra string
		if err = rows.Scan(&id, &extra); err != nil {
			return nil, nil, err
		}
		fields, err := parseObject([]byte(extra))
		if err != nil {
			return nil, nil, err
		}
		fields["id"] = id
		items = append(items, fields)
		if str(fields, "role") != "user" || fields["ephemeral"] == true {
			continue
		}
		text := str(fields, "text")
		if id == "" || text == "" {
			continue
		}
		out["chat:"+id] = object{"id": "chat:" + id, "kind": "self", "origin": "conversation", "title": "对话记录", "text": text}
	}
	return out, items, rows.Err()
}

func androidOwner(tx *sql.Tx) (string, error) {
	var user string
	err := tx.QueryRow("SELECT user_id FROM android_dossier WHERE slot=1").Scan(&user)
	if !errors.Is(err, sql.ErrNoRows) {
		return user, err
	}
	user = opaqueID(18)
	if _, err = tx.Exec("INSERT INTO users(id,token_hash) VALUES(?,randomblob(32))", user); err != nil {
		return "", err
	}
	_, err = tx.Exec("INSERT INTO android_dossier(slot,user_id) VALUES(1,?)", user)
	return user, err
}

func (s *Store) androidState(expected *int, in object, base string) (object, error) {
	var out object
	err := s.write(func(tx *sql.Tx) error {
		user, err := androidOwner(tx)
		if err != nil {
			return err
		}
		var revision int
		var metadata sql.NullString
		var sig, last []byte
		err = tx.QueryRow("SELECT revision,metadata,signature,last_input FROM android_dossier WHERE slot=1").Scan(&revision, &metadata, &sig, &last)
		if err != nil {
			return err
		}
		state, err := loadDossier(tx, user, metadata)
		if err != nil {
			return err
		}
		current := sha256.Sum256([]byte(encode(state)))
		token := fmt.Sprintf("%x", current[:])
		if state != nil && !bytes.Equal(current[:], sig) {
			revision++
			last = nil
			if _, err = tx.Exec("UPDATE android_dossier SET revision=?,signature=?,last_input=NULL WHERE slot=1", revision, current[:]); err != nil {
				return err
			}
		}
		if expected == nil {
			out = object{"ok": true, "revision": revision, "snapshot": token, "state": state}
			return nil
		}
		hash := sha256.Sum256([]byte(encode(in)))
		if (*expected == revision && bytes.Equal(hash[:], current[:])) || ((*expected == revision || *expected == revision-1) && bytes.Equal(hash[:], last)) {
			out = object{"ok": true, "revision": revision, "snapshot": token}
			return nil
		}
		if *expected != revision {
			return ErrConflict
		}
		// A revision alone is not enough: a client can hold r1/S1, adopt a
		// later tick's r2 without adopting S2, and then submit an edit based
		// on S1. The snapshot token ties the write to the exact state the
		// client read, so that stale base conflicts instead of overwriting S2.
		// Revision 0 is the first write and has no earlier state to tie to.
		// Exact retries were accepted above and do not need a base.
		if revision > 0 && base == "" {
			return ErrConflict
		}
		if base != "" && base != token {
			return ErrConflict
		}
		next, err := splitDossier(in)
		if err != nil {
			return err
		}
		if err = saveDossier(tx, user, state, next); err != nil {
			return err
		}
		state, err = loadDossier(tx, user, sql.NullString{String: encode(next.fields), Valid: true})
		if err != nil {
			return err
		}
		current = sha256.Sum256([]byte(encode(state)))
		token = fmt.Sprintf("%x", current[:])
		revision++
		if _, err = tx.Exec("UPDATE android_dossier SET revision=?,signature=?,last_input=? WHERE slot=1", revision, current[:], hash[:]); err != nil {
			return err
		}
		out = object{"ok": true, "revision": revision, "snapshot": token}
		return nil
	})
	return out, err
}

func historyTexts(raw string) ([]string, error) {
	items, err := decodeArray(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch h := item.(type) {
		case string:
			out = append(out, h)
		case object:
			out = append(out, str(h, "text"))
		default:
			return nil, ErrInvalid
		}
	}
	return out, nil
}

// First-reading tasks may cite source passages that were never extracted into a
// memory. Resolve both kinds of reference before dropping their cached prose.
// This only changes the current Android owner's metadata, never their goals.
func androidInvalidateFirstReading(tx *sql.Tx, user, factID, sourceID string, withdrawn bool) error {
	var raw sql.NullString
	if err := tx.QueryRow("SELECT metadata FROM android_dossier WHERE user_id=?", user).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if err := androidInvalidateGoalReviews(tx, user, mpIDSet(factID), mpIDSet(sourceID)); err != nil {
				return err
			}
			return androidInvalidateModelPreparations(tx, user, mpIDSet(factID), mpIDSet(sourceID), nil, nil)
		}
		return err
	}
	metadata := object{}
	if raw.Valid {
		var err error
		metadata, err = parseObject([]byte(raw.String))
		if err != nil {
			return err
		}
	}
	reveal, _ := metadata["reveal"].(object)
	task, _ := reveal["firstReading"].(object)
	sourceIDs := map[string]bool{}
	factIDs := map[string]bool{}
	if sourceID != "" {
		sourceIDs[sourceID] = true
		rows, err := tx.Query(`SELECT id FROM memories WHERE user_id=? AND (source_id=? OR
			EXISTS(SELECT 1 FROM json_each(evidence) WHERE json_extract(value,'$.sourceId')=?))`, user, sourceID, sourceID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			factIDs[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if factID != "" {
		factIDs[factID] = true
		memory, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, factID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if memory.SourceID != "" {
			sourceIDs[memory.SourceID] = true
		}
		for _, span := range memory.Evidence {
			sourceIDs[span.SourceID] = true
		}
	}
	// Renaming the same complete document cannot preserve stale references.
	// A different document containing the same sentence remains independent.
	documents, sourceHashes := map[string]bool{}, map[string]string{}
	documentHash := func(id string) (string, error) {
		if value, ok := sourceHashes[id]; ok {
			return value, nil
		}
		var body string
		err := tx.QueryRow("SELECT body FROM sources WHERE user_id=? AND id=?", user, id).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			sourceHashes[id] = ""
			return "", nil
		}
		if err != nil {
			return "", err
		}
		sourceHashes[id] = firstReadingHash("document", body)
		return sourceHashes[id], nil
	}
	for id := range sourceIDs {
		hash, err := documentHash(id)
		if err != nil {
			return err
		}
		if hash != "" {
			documents[hash] = true
		}
	}
	affectedReferences := func(references []object) (bool, error) {
		for _, ref := range references {
			if factIDs[str(ref, "id")] || factIDs[strings.TrimPrefix(str(ref, "key"), "f:")] || sourceIDs[str(ref, "sourceId")] {
				return true, nil
			}
			if id := str(ref, "sourceId"); id != "" {
				hash, err := documentHash(id)
				if err != nil {
					return false, err
				}
				if hash != "" && documents[hash] {
					return true, nil
				}
			}
		}
		return false, nil
	}
	selection, _ := objects(task["selection"])
	result, _ := task["result"].(object)
	citations, _ := objects(result["citations"])
	affected, err := affectedReferences(append(selection, citations...))
	if err != nil {
		return err
	}
	taskID := str(task, "id")
	if affected {
		delete(task, "selection")
		delete(task, "result")
		delete(task, "correctionId")
		task["status"] = "stale"
		task["error"] = "参考资料已变化，请重新选择当前资料后阅读。"
		if _, err := tx.Exec("UPDATE android_dossier SET metadata=? WHERE user_id=?", encode(metadata), user); err != nil {
			return err
		}
	}
	// Chat can cite an unextracted source even without an initial reading task.
	// Preserve the user's messages and unrelated assistant turns.
	rows, err := tx.Query(`SELECT id,extra FROM android_fields WHERE user_id=? AND kind='conversation'
		AND json_extract(extra,'$.role')='assistant' AND (json_type(extra,'$.readingReferences')='array'
		OR json_type(extra,'$.discoveryReferences')='array'
		OR (json_extract(extra,'$.kind')='first-discovery' AND json_extract(extra,'$.firstReadingId')=?))`, user, taskID)
	if err != nil {
		return err
	}
	type messageRef struct {
		id     string
		fields object
	}
	var messages []messageRef
	for rows.Next() {
		var id, extra string
		if err := rows.Scan(&id, &extra); err != nil {
			rows.Close()
			return err
		}
		fields, err := parseObject([]byte(extra))
		if err != nil {
			rows.Close()
			return err
		}
		messages = append(messages, messageRef{id, fields})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, message := range messages {
		refs, _ := objects(message.fields["readingReferences"])
		linked, err := affectedReferences(refs)
		if err != nil {
			return err
		}
		interpretationChanged := false
		for _, discovery := range firstReadingDiscoveryReferences(message.fields) {
			citations, _ := objects(discovery["citations"])
			changed, err := affectedReferences(citations)
			if err != nil {
				return err
			}
			linked = linked || changed
			interpretationChanged = interpretationChanged || (affected && taskID != "" && str(discovery, "id") == taskID)
		}
		linked = linked || (affected && taskID != "" && str(message.fields, "kind") == "first-discovery" && str(message.fields, "firstReadingId") == taskID)
		if !linked && !interpretationChanged {
			continue
		}
		// A changed input can invalidate the interpretation without withdrawing
		// any passage this answer actually cited. Preserve that marked reply.
		if withdrawn && linked {
			_, err = tx.Exec("DELETE FROM android_fields WHERE user_id=? AND kind='conversation' AND id=?", user, message.id)
		} else {
			_, err = tx.Exec(`UPDATE android_fields SET extra=json_set(json_remove(extra,'$.readingReferences'),'$.basisCorrected',json('true'))
				WHERE user_id=? AND kind='conversation' AND id=?`, user, message.id)
		}
		if err != nil {
			return err
		}
	}
	if err := androidInvalidateGoalReviews(tx, user, factIDs, sourceIDs); err != nil {
		return err
	}
	if err := androidInvalidateModelPreparations(tx, user, factIDs, sourceIDs, documents, documentHash); err != nil {
		return err
	}
	return androidPropagateDiscoveryConversation(tx, user)
}

// Service-side edits invalidate the same derived UI records as an Android edit.
func androidChangedFact(tx *sql.Tx, user, id string, withdrawn bool) error {
	if err := androidInvalidateFirstReading(tx, user, id, "", withdrawn); err != nil {
		return err
	}
	linked := ` WHERE user_id=? AND kind='conversation' AND json_extract(extra,'$.role')='assistant'
		AND EXISTS(SELECT 1 FROM json_each(extra,'$.factIds') WHERE value=?)`
	action := "UPDATE android_fields SET extra=json_set(extra,'$.basisCorrected',json('true'))"
	if withdrawn {
		action = "DELETE FROM android_fields"
	}
	if _, err := tx.Exec(action+linked, user, id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE android_dossier SET metadata=json_set(metadata,'$.profile',null) WHERE user_id=?", user); err != nil {
		return err
	}
	for _, path := range []string{"$.reveal.opening", "$.reveal.future"} {
		if _, err := tx.Exec(`UPDATE android_dossier SET metadata=json_remove(metadata,?)
			WHERE user_id=? AND EXISTS(SELECT 1 FROM json_each(metadata,?) WHERE json_extract(value,'$.id')=?)`, path, user, path+".citations", id); err != nil {
			return err
		}
	}
	if !withdrawn {
		if _, err := tx.Exec(`UPDATE android_fields SET extra=json_set(json_remove(extra,'$.retainedReading'),'$.source','本人确认') WHERE user_id=? AND kind='facts' AND id=? AND json_extract(extra,'$.retainedReading')`, user, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE android_fields SET extra=json_set(extra,'$.at',strftime('%Y-%m-%dT%H:%M:%fZ','now')) WHERE user_id=? AND kind='facts' AND id=?`, user, id)
		return err
	}
	return nil
}

func androidForgetReferences(tx *sql.Tx, user, id string) error {
	if _, err := tx.Exec(`DELETE FROM proposals WHERE user_id=? AND basis_id=? AND id IN(
		SELECT id FROM android_fields WHERE user_id=? AND kind='proposals' AND json_extract(extra,'$.titleFromFact'))`, user, id, user); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM android_fields WHERE user_id=? AND kind='proposals' AND NOT EXISTS(
		SELECT 1 FROM proposals p WHERE p.user_id=android_fields.user_id AND p.id=android_fields.id)`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE android_fields SET extra=json_set(extra,'$.factIds',
		(SELECT json_group_array(value) FROM json_each(extra,'$.factIds') WHERE value!=?))
		WHERE user_id=? AND kind='proposals' AND EXISTS(SELECT 1 FROM json_each(extra,'$.factIds') WHERE value=?)`, id, user, id); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE android_dossier SET metadata=json_remove(metadata,'$.reveal.firstId')
		WHERE user_id=? AND json_extract(metadata,'$.reveal.firstId')=?`, user, id)
	return err
}

func androidRemoveSource(tx *sql.Tx, user, id string) error {
	if err := androidInvalidateFirstReading(tx, user, "", id, true); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM android_fields WHERE user_id=? AND ((kind='sources' AND id=?) OR
		(kind='conversation' AND json_extract(extra,'$.kind')='calendar-change' AND json_extract(extra,'$.sourceId')=?) OR
		(kind='dismissed' AND json_extract(id,'$[0]')=?) OR
		(kind='facts' AND id IN(SELECT id FROM memories WHERE user_id=? AND source_id=?)))`, user, id, id, id, user, id); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE android_dossier SET metadata=json_remove(metadata,'$.reveal.lastSource') WHERE user_id=? AND json_extract(metadata,'$.reveal.lastSource')=?`, user, id)
	return err
}
