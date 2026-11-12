package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("memory changed since it was opened")
	ErrInvalid  = errors.New("invalid input")
)

// 单位开放（与前端 core.js 同步，发起人 2026-11-10）：任意 1-12 字或空（里程碑目标）。
func validUnit(u string) bool { return utf8.RuneCountInString(u) <= 12 }

type Store struct {
	db     *sql.DB
	dbPath string
}

type Source struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Origin string `json:"origin,omitempty"`
	Text   string `json:"text"`
	At     string `json:"at,omitempty"`
}

type EvidenceSpan struct {
	SourceID string `json:"sourceId"`
	Quote    string `json:"quote"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

type Memory struct {
	ID              string         `json:"id"`
	SourceID        string         `json:"sourceId"`
	Text            string         `json:"text"`
	Quote           string         `json:"quote"`
	Category        string         `json:"category,omitempty"`
	Attribution     string         `json:"attribution,omitempty"`
	Confirmed       bool           `json:"confirmed"`
	Version         int            `json:"version"`
	Alternatives    []Finding      `json:"alternatives,omitempty"`
	Evidence        []EvidenceSpan `json:"evidence,omitempty"`
	RetainedReading bool           `json:"retainedReading,omitempty"`
}

type ReadingRevision struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type Proposal struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Basis     string         `json:"basis,omitempty"`
	BasisRefs []GoalBasisRef `json:"basisRefs,omitempty"`
	At        string         `json:"at"`
}

type GoalEntry struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
	Note   string  `json:"note"`
	At     string  `json:"at"`
}

type GoalBasis struct {
	ID              string `json:"id"`
	AcceptedVersion int    `json:"acceptedVersion"`
	CurrentVersion  int    `json:"currentVersion,omitempty"`
	CurrentText     string `json:"currentText,omitempty"`
}

type GoalMethod struct {
	Origin  string `json:"origin"`
	FactID  string `json:"factId,omitempty"`
	Version int    `json:"version,omitempty"`
	Text    string `json:"text"`
}

type Goal struct {
	ID              string             `json:"id"`
	ProposalID      string             `json:"proposalId,omitempty"`
	ApprovedBasis   []GoalBasisRef     `json:"approvedBasis,omitempty"`
	ApprovedMethods []GoalMethod       `json:"approvedMethods,omitempty"`
	CurrentBasis    []CurrentGoalBasis `json:"currentBasis,omitempty"`
	Basis           *GoalBasis         `json:"basis,omitempty"`
	Title           string             `json:"title"`
	Step            string             `json:"step"`
	Unit            string             `json:"unit"`
	Entries         []GoalEntry        `json:"entries"`
	At              string             `json:"at"`
}

type ReportRow struct {
	GoalID string   `json:"goalId"`
	Title  string   `json:"title"`
	Step   string   `json:"step"`
	Unit   string   `json:"unit"`
	Count  int      `json:"count"`
	Amount float64  `json:"amount"`
	Notes  []string `json:"notes"`
}

type Report struct {
	ID   string      `json:"id"`
	At   string      `json:"at"`
	From string      `json:"from"`
	Rows []ReportRow `json:"rows"`
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS sources (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
	kind TEXT NOT NULL, origin TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, body TEXT NOT NULL, digest BLOB NOT NULL,
	created_at TEXT NOT NULL DEFAULT '',
	UNIQUE(user_id,id), UNIQUE(user_id,kind,origin,title,digest)
);
CREATE TABLE IF NOT EXISTS checked_note_readings (
	source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE, complete INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS memories (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), source_id TEXT,
	current_text TEXT NOT NULL, quote TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
	category TEXT NOT NULL DEFAULT 'state', attribution TEXT NOT NULL DEFAULT 'self',
	confirmed INTEGER NOT NULL DEFAULT 0, version INTEGER NOT NULL DEFAULT 1,
	history TEXT NOT NULL DEFAULT '[]', alternatives TEXT NOT NULL DEFAULT '[]',
	evidence TEXT NOT NULL DEFAULT '[]',
	FOREIGN KEY(user_id,source_id) REFERENCES sources(user_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS memory_origin ON memories(user_id,source_id,start_byte,end_byte);
CREATE TABLE IF NOT EXISTS dismissed (
	user_id TEXT NOT NULL, source_id TEXT NOT NULL, start_byte INTEGER NOT NULL, end_byte INTEGER NOT NULL,
	PRIMARY KEY(user_id,source_id,start_byte,end_byte),
	FOREIGN KEY(user_id,source_id) REFERENCES sources(user_id,id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS proposals (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
	title TEXT NOT NULL, basis_id TEXT, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS goals (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
	title TEXT NOT NULL, step TEXT NOT NULL, unit TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS goal_acceptances (
	goal_id TEXT PRIMARY KEY REFERENCES goals(id) ON DELETE CASCADE,
	proposal_id TEXT NOT NULL,
	memory_id TEXT NOT NULL, accepted_version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS goal_entries (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
	goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
	amount REAL NOT NULL, note TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reports (
	id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
	from_at TEXT NOT NULL, created_at TEXT NOT NULL, rows_json TEXT NOT NULL DEFAULT '[]'
);`

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, ErrInvalid
	}
	s := &Store{dbPath: path}
	dsn := "file::memory:"
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Chmod(0600); err != nil {
			f.Close()
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute}).String()
	}
	db, err := sql.Open("sqlite", dsn+sqliteOptions)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(storeSchema + dossierSchema + discoverySchema + vaultSchema + selfHostPreparationSchema + goalReviewSchema + artifactSchema); err != nil {
		db.Close()
		return nil, err
	}
	s.db = db
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) nowOrUTC() time.Time { return time.Now().UTC() }

func opaqueID(size int) string {
	b := make([]byte, size)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Store) CreateUser() (string, error) {
	token := opaqueID(32)
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.Exec("INSERT INTO users(id,token_hash) VALUES(?,?)", opaqueID(18), sum[:])
	if err != nil {
		return "", err
	}
	return token, nil
}

func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) User(token string) (string, error) {
	var id string
	sum := sha256.Sum256([]byte(token))
	err := s.db.QueryRow("SELECT id FROM users WHERE token_hash=?", sum[:]).Scan(&id)
	return id, missing(err)
}

func (s *Store) write(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

const sourceColumns = "id, title, kind, origin, body, created_at"

type rowScanner interface{ Scan(...any) error }

func readSource(row rowScanner) (Source, error) {
	var src Source
	err := row.Scan(&src.ID, &src.Title, &src.Kind, &src.Origin, &src.Text, &src.At)
	return src, missing(err)
}

func (s *Store) Source(user, id string) (Source, error) {
	return readSource(s.db.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, id))
}

func (s *Store) Sources(user string) ([]Source, error) {
	rows, err := s.db.Query("SELECT id,title,kind,origin,created_at FROM sources WHERE user_id=? ORDER BY rowid DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Source{}
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Title, &src.Kind, &src.Origin, &src.At); err != nil {
			return nil, err
		}
		list = append(list, src)
	}
	return list, rows.Err()
}

func (s *Store) Import(user, kind, title, body string, readings []Finding) (Source, error) {
	return s.ImportFrom(user, kind, "", title, body, readings)
}

func (s *Store) ImportFrom(user, kind, origin, title, body string, readings []Finding) (Source, error) {
	return s.storeSpans(user, kind, origin, title, body, locateFindings(body, readings), false)
}

// Model and manually supplied findings have a quotation but no source position.
// The same words can appear in a quoted reply and again as the author's own;
// evidence must point at the authored occurrence when one exists.
func locateFindings(body string, readings []Finding) []ReadingSpan {
	spans := make([]ReadingSpan, 0, len(readings))
	for _, f := range readings {
		f.Quote = strings.TrimSpace(f.Quote)
		start := authoredOccurrence(body, f.Quote)
		spans = append(spans, ReadingSpan{Finding: f, Start: start, End: start + len(f.Quote)})
	}
	return spans
}

func authoredOccurrence(body, quote string) int {
	first := strings.Index(body, quote)
	if first < 0 || !strings.Contains(body[:first], ">") {
		return first
	}
	for at := 0; at+len(quote) <= len(body); {
		found := strings.Index(body[at:], quote)
		if found < 0 {
			break
		}
		at += found
		lineStart := strings.LastIndexByte(body[:at], '\n') + 1
		line := body[lineStart:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if !quotedLine.MatchString(line) {
			return at
		}
		at += len(quote)
	}
	return first
}

func (s *Store) importMaterial(user, kind, origin, title, body string) (Source, error) {
	return s.storeSpans(user, kind, origin, title, body, ReadMaterialSpans(kind, body), true)
}

func (s *Store) storeSpans(user, kind, origin, title, body string, readings []ReadingSpan, fullReading bool) (Source, error) {
	kind, origin, title = strings.TrimSpace(kind), strings.TrimSpace(origin), strings.TrimSpace(title)
	if kind == "" || title == "" || strings.TrimSpace(body) == "" {
		return Source{}, ErrInvalid
	}
	digest := sha256.Sum256([]byte(body))
	now := time.Now().UTC().Format(time.RFC3339)
	src := Source{Kind: kind, Origin: origin, Title: title, Text: body, At: now}
	err := s.write(func(tx *sql.Tx) error {
		err := tx.QueryRow("SELECT id, created_at FROM sources WHERE user_id=? AND kind=? AND origin=? AND title=? AND digest=?", user, kind, origin, title, digest[:]).Scan(&src.ID, &src.At)
		newSource := errors.Is(err, sql.ErrNoRows)
		if newSource {
			src.ID = opaqueID(18)
			_, err = tx.Exec("INSERT INTO sources(id,user_id,kind,origin,title,body,digest,created_at) VALUES(?,?,?,?,?,?,?,?)", src.ID, user, kind, origin, title, body, digest[:], now)
		}
		if err != nil {
			return err
		}
		if fullReading && kind == "self" {
			if !newSource {
				var complete bool
				err = tx.QueryRow("SELECT complete FROM checked_note_readings WHERE source_id=?", src.ID).Scan(&complete)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if complete {
					return nil
				}
			}
			if _, err = tx.Exec("INSERT OR REPLACE INTO checked_note_readings VALUES(?,1)", src.ID); err != nil {
				return err
			}
		}
		if newSource && fullReading {
			// The offline reader emits disjoint verified ranges in source order.
			for _, r := range readings {
				if err = insertReading(tx, user, src, r); err != nil {
					return err
				}
			}
			return nil
		}
		return applyReadingSpans(tx, user, src, readings)
	})
	if err != nil {
		return Source{}, err
	}
	return src, nil
}

func (s *Store) AddReadings(user, sourceID string, readings []Finding) error {
	return s.write(func(tx *sql.Tx) error {
		src, err := readSource(tx.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, sourceID))
		if err != nil {
			return err
		}
		return applyReadingSpans(tx, user, src, locateFindings(src.Text, readings))
	})
}

func applyReadingSpans(tx *sql.Tx, user string, src Source, readings []ReadingSpan) error {
	for _, r := range readings {
		r.Text = strings.TrimSpace(r.Text)
		start, end := r.Start, r.End
		if r.Text == "" || r.Quote == "" || start < 0 || end < start || end > len(src.Text) || src.Text[start:end] != r.Quote {
			return ErrInvalid
		}
		var blocked bool
		err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM dismissed WHERE user_id=? AND source_id=? AND start_byte<? AND end_byte>?)", user, src.ID, end, start).Scan(&blocked)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		// A reading whose span overlaps more than one existing memory cannot be
		// assigned to either without conflating them. Whether a quote really
		// covers two independent statements is for the model to judge, not a
		// splitter; the structural check below is language-independent.
		var overlapping int
		err = tx.QueryRow("SELECT COUNT(*) FROM memories WHERE user_id=? AND source_id=? AND start_byte<? AND end_byte>?",
			user, src.ID, end, start).Scan(&overlapping)
		if err != nil {
			return err
		}
		if overlapping > 1 {
			continue
		}
		m, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND source_id=? AND start_byte<? AND end_byte>? ORDER BY confirmed DESC, rowid LIMIT 1", user, src.ID, end, start))
		if errors.Is(err, ErrNotFound) {
			if err = insertReading(tx, user, src, r); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if m.Confirmed || m.Text == r.Text {
			continue
		}
		duplicate := false
		for _, alt := range m.Alternatives {
			if alt.Text == r.Text && alt.Quote == r.Quote {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		options, err := json.Marshal(append(m.Alternatives, Finding{Text: r.Text, Quote: r.Quote}))
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE memories SET alternatives=?,start_byte=min(start_byte,?),end_byte=max(end_byte,?),version=version+1 WHERE user_id=? AND id=?", string(options), start, end, user, m.ID); err != nil {
			return err
		}
	}
	return nil
}

func insertReading(tx *sql.Tx, user string, src Source, r ReadingSpan) error {
	evidence, err := json.Marshal([]EvidenceSpan{{SourceID: src.ID, Quote: r.Quote, Start: r.Start, End: r.End}})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO memories(id,user_id,source_id,current_text,quote,start_byte,end_byte,category,attribution,evidence) VALUES(?,?,?,?,?,?,?,?,?,?)",
		opaqueID(18), user, src.ID, r.Text, r.Quote, r.Start, r.End, inferCategory(r.Text, src.Kind), inferAttribution(src.Kind, false), string(evidence))
	return err
}

// AddCrossClaim records an insight backed by one or more verbatim evidence spans across sources.
func (s *Store) AddCrossClaim(user, text string, spans []EvidenceSpan) (Memory, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(spans) == 0 || utf8.RuneCountInString(text) > 1000 {
		return Memory{}, ErrInvalid
	}
	var out Memory
	err := s.write(func(tx *sql.Tx) error {
		verified := make([]EvidenceSpan, 0, len(spans))
		primaryKind := "self"
		for i, sp := range spans {
			sp.Quote = strings.TrimSpace(sp.Quote)
			if sp.SourceID == "" || sp.Quote == "" {
				return ErrInvalid
			}
			src, err := readSource(tx.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, sp.SourceID))
			if err != nil {
				return err
			}
			if i == 0 {
				primaryKind = src.Kind
			}
			start := strings.Index(src.Text, sp.Quote)
			if start < 0 {
				return ErrInvalid
			}
			end := start + len(sp.Quote)
			var blocked bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM dismissed WHERE user_id=? AND source_id=? AND start_byte<? AND end_byte>?)", user, src.ID, end, start).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				return ErrInvalid
			}
			verified = append(verified, EvidenceSpan{SourceID: src.ID, Quote: sp.Quote, Start: start, End: end})
		}
		primary := verified[0]
		cat := inferCategory(text, primaryKind)
		attr := inferAttribution(primaryKind, false)
		evJSON, err := json.Marshal(verified)
		if err != nil {
			return err
		}
		id := opaqueID(18)
		_, err = tx.Exec("INSERT INTO memories(id,user_id,source_id,current_text,quote,start_byte,end_byte,category,attribution,evidence) VALUES(?,?,?,?,?,?,?,?,?,?)",
			id, user, primary.SourceID, text, primary.Quote, primary.Start, primary.End, cat, attr, string(evJSON))
		if err != nil {
			return err
		}
		out = Memory{
			ID: id, SourceID: primary.SourceID, Text: text, Quote: primary.Quote,
			Category: cat, Attribution: attr, Confirmed: false, Version: 1, Evidence: verified,
		}
		return nil
	})
	if err != nil {
		return Memory{}, err
	}
	return out, nil
}

const memoryColumns = "id, COALESCE(source_id,''), current_text, quote, category, attribution, confirmed, version, alternatives, history, evidence"

func readMemory(row rowScanner) (Memory, []string, error) {
	var m Memory
	var alts, history, ev string
	if err := row.Scan(&m.ID, &m.SourceID, &m.Text, &m.Quote, &m.Category, &m.Attribution, &m.Confirmed, &m.Version, &alts, &history, &ev); err != nil {
		return m, nil, missing(err)
	}
	if err := json.Unmarshal([]byte(alts), &m.Alternatives); err != nil {
		return m, nil, err
	}
	if ev != "" && ev != "[]" {
		_ = json.Unmarshal([]byte(ev), &m.Evidence)
	}
	old, err := historyTexts(history)
	m.RetainedReading = m.SourceID == "" && !m.Confirmed
	return m, old, err
}

func (s *Store) Portrait(user string) ([]Memory, error) {
	rows, err := s.db.Query("SELECT "+memoryColumns+" FROM memories WHERE user_id=? ORDER BY rowid DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Memory{}
	for rows.Next() {
		m, _, err := readMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Correct(user, id, text string, version int) (Memory, error) {
	text = strings.TrimSpace(text)
	if text == "" || version < 1 {
		return Memory{}, ErrInvalid
	}
	var m Memory
	err := s.write(func(tx *sql.Tx) error {
		var err error
		m, _, err = readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE id=? AND user_id=?", id, user))
		if err != nil {
			return err
		}
		if m.Version != version {
			return ErrConflict
		}
		if m.Text == text && m.Confirmed {
			return nil
		}
		for _, candidate := range m.Alternatives {
			if text == candidate.Text {
				m.Quote = candidate.Quote
				break
			}
		}
		var previous string
		if err = tx.QueryRow("SELECT history FROM memories WHERE id=? AND user_id=?", id, user).Scan(&previous); err != nil {
			return err
		}
		if m.Text != text {
			if err = tx.QueryRow(`SELECT json_insert(COALESCE(NULLIF(?, 'null'),'[]'),'$[#]',json_object('text',?,'at',
				COALESCE((SELECT json_extract(extra,'$.at') FROM android_fields WHERE user_id=? AND kind='facts' AND id=?),'')))`, previous, m.Text, user, id).Scan(&previous); err != nil {
				return err
			}
		}
		// Confirming the same words also advances the version and changes their
		// provenance; a pending model request must not use its previous snapshot.
		if err = androidChangedFact(tx, user, id, false); err != nil {
			return err
		}
		cat := inferCategory(text, "self")
		m.Text, m.Category, m.Attribution, m.Confirmed, m.Version, m.Alternatives = text, cat, "confirmed", true, version+1, nil
		m.RetainedReading = false
		_, err = tx.Exec("UPDATE memories SET current_text=?,quote=?,category=?,attribution='confirmed',confirmed=1,version=?,alternatives='[]',history=? WHERE user_id=? AND id=?", m.Text, m.Quote, m.Category, m.Version, string(previous), user, id)
		if err != nil {
			return err
		}
		return refreshDiscoveries(tx, user)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

func (s *Store) Forget(user, id string) error {
	return s.write(func(tx *sql.Tx) error {
		m, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, id))
		if err != nil {
			return err
		}
		if len(m.Evidence) == 0 && m.SourceID != "" {
			_, err = tx.Exec("INSERT OR IGNORE INTO dismissed SELECT user_id,source_id,start_byte,end_byte FROM memories WHERE user_id=? AND id=?", user, id)
			if err != nil {
				return err
			}
		}
		for _, sp := range m.Evidence {
			_, err = tx.Exec("INSERT OR IGNORE INTO dismissed VALUES(?,?,?,?)", user, sp.SourceID, sp.Start, sp.End)
			if err != nil {
				return err
			}
		}
		if err = androidChangedFact(tx, user, id, true); err != nil {
			return err
		}
		if err = androidForgetReferences(tx, user, id); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM android_fields WHERE user_id=? AND kind='facts' AND id=?", user, id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE proposals SET basis_id=NULL WHERE user_id=? AND basis_id=?", user, id); err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM memories WHERE user_id=? AND id=?", user, id)
		if err != nil {
			return err
		}
		return refreshDiscoveries(tx, user)
	})
}

func (s *Store) RemoveSource(user, sourceID string, keepConfirmed []string, keepReadings ...ReadingRevision) error {
	return s.write(func(tx *sql.Tx) error {
		src, err := readSource(tx.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, sourceID))
		if err != nil {
			return err
		}
		// Read the old evidence before detaching a surviving memory from this
		// source. The transaction rolls these metadata changes back on failure.
		if err = androidRememberReadingSuppressions(tx, user, src); err != nil {
			return err
		}
		if err = androidInvalidateFirstReading(tx, user, "", sourceID, true); err != nil {
			return err
		}
		requested := make(map[string]int, len(keepReadings))
		for _, ref := range keepReadings {
			if ref.ID == "" || ref.Version < 1 || (requested[ref.ID] != 0 && requested[ref.ID] != ref.Version) {
				return ErrInvalid
			}
			requested[ref.ID] = ref.Version
		}
		keepSet := make(map[string]bool, len(keepConfirmed))
		for _, id := range keepConfirmed {
			keepSet[id] = true
		}
		// Promote any memory that still has surviving evidence from another source, or handle retention/cleanup.
		rows, err := tx.Query("SELECT "+memoryColumns+" FROM memories WHERE user_id=?", user)
		if err != nil {
			return err
		}
		var all []Memory
		superseded := map[string]bool{}
		for rows.Next() {
			m, old, err := readMemory(rows)
			if err != nil {
				rows.Close()
				return err
			}
			all = append(all, m)
			if len(requested) > 0 && m.Confirmed && len(old) > 0 {
				superseded[readingKey(m.Quote)] = true
				for _, prior := range old {
					superseded[readingKey(prior)] = true
				}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		var safe map[EvidenceSpan]bool
		if len(requested) > 0 {
			safe = retainableSpans(src)
			// Forgetting one copy must also cover an identical quote kept by a different source.
			rows, err := tx.Query(`SELECT d.source_id,d.start_byte,d.end_byte,substr(CAST(s.body AS BLOB), d.start_byte+1, d.end_byte-d.start_byte)
				FROM dismissed d JOIN sources s ON s.id=d.source_id AND s.user_id=d.user_id WHERE d.user_id=?`, user)
			if err != nil {
				return err
			}
			for rows.Next() {
				var origin, forgotten string
				var start, end int
				if err := rows.Scan(&origin, &start, &end, &forgotten); err != nil {
					rows.Close()
					return err
				}
				superseded[readingKey(forgotten)] = true
				if origin == sourceID {
					for span := range safe {
						if span.Start < end && start < span.End {
							delete(safe, span)
						}
					}
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		for _, m := range all {
			var remaining []EvidenceSpan
			touched := m.SourceID == sourceID
			for _, sp := range m.Evidence {
				if sp.SourceID == sourceID {
					touched = true
				} else {
					remaining = append(remaining, sp)
				}
			}
			if !touched {
				continue
			}
			v, selected := requested[m.ID]
			if selected {
				if m.Confirmed || v != m.Version {
					return ErrConflict
				}
				delete(requested, m.ID)
			}
			if len(remaining) > 0 {
				nextPrimary := remaining[0]
				evJSON, _ := json.Marshal(remaining)
				if _, err := tx.Exec("UPDATE memories SET source_id=?,quote=?,start_byte=?,end_byte=?,evidence=?,version=version+1 WHERE user_id=? AND id=?",
					nextPrimary.SourceID, nextPrimary.Quote, nextPrimary.Start, nextPrimary.End, string(evJSON), user, m.ID); err != nil {
					return err
				}
				continue
			}
			if err = androidChangedFact(tx, user, m.ID, true); err != nil {
				return err
			}
			if selected || (m.Confirmed && keepSet[m.ID]) {
				attribute := "confirmed"
				if selected {
					if !canDetachReading(src, m, safe, superseded) {
						return ErrConflict
					}
					attribute = m.Attribution
				}
				if _, err := tx.Exec("UPDATE memories SET source_id=NULL,quote='',start_byte=0,end_byte=0,attribution=?,history='[]',alternatives='[]',evidence='[]',version=version+1 WHERE user_id=? AND id=?", attribute, user, m.ID); err != nil {
					return err
				}
			} else {
				if err = androidForgetReferences(tx, user, m.ID); err != nil {
					return err
				}
				if _, err := tx.Exec("UPDATE proposals SET basis_id=NULL WHERE user_id=? AND basis_id=?", user, m.ID); err != nil {
					return err
				}
			}
		}
		if len(requested) > 0 {
			return ErrConflict
		}
		if err = androidRemoveSource(tx, user, sourceID); err != nil {
			return err
		}
		result, err := tx.Exec("DELETE FROM sources WHERE user_id=? AND id=?", user, sourceID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n == 0 {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return refreshDiscoveries(tx, user)
	})
}

func (s *Store) Propose(user, title, basisID string) (Proposal, error) {
	title, basisID = strings.TrimSpace(title), strings.TrimSpace(basisID)
	if title == "" || utf8.RuneCountInString(title) > 160 {
		return Proposal{}, ErrInvalid
	}
	now := time.Now().UTC().Format(time.RFC3339)
	p := Proposal{ID: opaqueID(18), Title: title, Basis: basisID, At: now}
	err := s.write(func(tx *sql.Tx) error {
		var basis any
		if basisID != "" {
			var exists bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM memories WHERE user_id=? AND id=?)", user, basisID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			basis = basisID
		}
		_, err := tx.Exec("INSERT INTO proposals(id,user_id,title,basis_id,created_at) VALUES(?,?,?,?,?)", p.ID, user, p.Title, basis, p.At)
		return err
	})
	return p, err
}

func (s *Store) Proposals(user string) ([]Proposal, error) {
	rows, err := s.db.Query("SELECT id,title,COALESCE(basis_id,''),created_at FROM proposals WHERE user_id=? ORDER BY rowid DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.Title, &p.Basis, &p.At); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		var raw string
		e := s.db.QueryRow("SELECT refs FROM proposal_basis_refs WHERE proposal_id=?", out[i].ID).Scan(&raw)
		if e == nil {
			if e = json.Unmarshal([]byte(raw), &out[i].BasisRefs); e != nil {
				return nil, e
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	return out, nil
}

func (s *Store) AcceptProposal(user, proposalID, title, step, unit string) (Goal, error) {
	return s.AcceptProposalWithBasis(user, proposalID, title, step, unit, nil)
}

func (s *Store) AcceptProposalWithBasis(user, proposalID, title, step, unit string, basis *ReadingRevision) (Goal, error) {
	title, step, unit = strings.TrimSpace(title), strings.TrimSpace(step), strings.TrimSpace(unit)
	if title == "" || utf8.RuneCountInString(title) > 160 || step == "" || utf8.RuneCountInString(step) > 300 || !validUnit(unit) {
		return Goal{}, ErrInvalid
	}
	if basis != nil && (basis.ID == "" || basis.Version < 1) {
		return Goal{}, ErrInvalid
	}
	now := time.Now().UTC().Format(time.RFC3339)
	g := Goal{ID: opaqueID(18), ProposalID: proposalID, Title: title, Step: step, Unit: unit, Entries: []GoalEntry{}, At: now}
	err := s.write(func(tx *sql.Tx) error {
		var approved GoalBasis
		if basis != nil {
			err := tx.QueryRow(`SELECT COALESCE(m.id,''), COALESCE(m.version,0), COALESCE(m.current_text,'')
				FROM proposals p LEFT JOIN memories m ON m.id=p.basis_id AND m.user_id=p.user_id
				WHERE p.user_id=? AND p.id=?`, user, proposalID).Scan(&approved.ID, &approved.CurrentVersion, &approved.CurrentText)
			if err != nil {
				return missing(err)
			}
			if basis.ID != approved.ID || basis.Version != approved.CurrentVersion {
				return ErrConflict
			}
			approved.AcceptedVersion = basis.Version
			g.Basis = &approved
		}
		res, err := tx.Exec("DELETE FROM proposals WHERE user_id=? AND id=?", user, proposalID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec("INSERT INTO goals(id,user_id,title,step,unit,created_at) VALUES(?,?,?,?,?,?)", g.ID, user, g.Title, g.Step, g.Unit, g.At)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO goal_acceptances(goal_id,proposal_id,memory_id,accepted_version) VALUES(?,?,?,?)", g.ID, proposalID, approved.ID, approved.AcceptedVersion)
		return err
	})
	if err != nil {
		return Goal{}, err
	}
	return g, nil
}

func validGoalAmount(amount float64) bool {
	return !math.IsNaN(amount) && !math.IsInf(amount, 0) && amount > 0 && amount <= 100000
}

func roundGoalTotal(sum float64) float64 {
	return math.Round(sum*1000) / 1000
}

func (s *Store) RecordGoal(user, goalID string, amount float64, note string) (GoalEntry, error) {
	note = strings.TrimSpace(note)
	if !validGoalAmount(amount) || utf8.RuneCountInString(note) > 280 {
		return GoalEntry{}, ErrInvalid
	}
	now := time.Now().UTC().Format(time.RFC3339)
	entry := GoalEntry{ID: opaqueID(18), Amount: amount, Note: note, At: now}
	err := s.write(func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM goals WHERE user_id=? AND id=?)", user, goalID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		_, err := tx.Exec("INSERT INTO goal_entries(id,user_id,goal_id,amount,note,created_at) VALUES(?,?,?,?,?,?)",
			entry.ID, user, goalID, entry.Amount, entry.Note, entry.At)
		return err
	})
	return entry, err
}

func goalProgressInTx(tx *sql.Tx, user, goalID, from, until string) (int, float64, []string, error) {
	rows, err := tx.Query("SELECT amount, note FROM goal_entries WHERE user_id=? AND goal_id=? AND created_at>=? AND created_at<=? ORDER BY rowid DESC",
		user, goalID, from, until)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	count := 0
	sum := 0.0
	notes := []string{}
	for rows.Next() {
		var amt float64
		var n string
		if err := rows.Scan(&amt, &n); err != nil {
			return 0, 0, nil, err
		}
		count++
		sum += amt
		if n != "" {
			notes = append(notes, n)
		}
	}
	return count, roundGoalTotal(sum), notes, rows.Err()
}

func (s *Store) ChangeGoalEntry(user, goalID, entryID string, amount *float64, note *string) error {
	if amount != nil && !validGoalAmount(*amount) {
		return ErrInvalid
	}
	if note != nil && utf8.RuneCountInString(strings.TrimSpace(*note)) > 280 {
		return ErrInvalid
	}
	return s.write(func(tx *sql.Tx) error {
		var entryAt, oldNote string
		var oldAmt float64
		if err := tx.QueryRow("SELECT amount,note,created_at FROM goal_entries WHERE user_id=? AND goal_id=? AND id=?", user, goalID, entryID).Scan(&oldAmt, &oldNote, &entryAt); err != nil {
			return missing(err)
		}
		if amount == nil {
			if _, err := tx.Exec("DELETE FROM goal_entries WHERE user_id=? AND goal_id=? AND id=?", user, goalID, entryID); err != nil {
				return err
			}
		} else {
			nextNote := oldNote
			if note != nil {
				nextNote = strings.TrimSpace(*note)
			}
			if _, err := tx.Exec("UPDATE goal_entries SET amount=?,note=? WHERE user_id=? AND goal_id=? AND id=?", *amount, nextNote, user, goalID, entryID); err != nil {
				return err
			}
		}
		// Keep saved weekly reports covering this entry's timestamp synchronized.
		repRows, err := tx.Query("SELECT id,from_at,created_at,rows_json FROM reports WHERE user_id=? AND from_at<=? AND created_at>=?", user, entryAt, entryAt)
		if err != nil {
			return err
		}
		type repRecord struct{ id, from, at, raw string }
		var affected []repRecord
		for repRows.Next() {
			var r repRecord
			if err := repRows.Scan(&r.id, &r.from, &r.at, &r.raw); err != nil {
				repRows.Close()
				return err
			}
			affected = append(affected, r)
		}
		repRows.Close()
		for _, r := range affected {
			var parsed []map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.raw), &parsed); err != nil {
				return err
			}
			for _, row := range parsed {
				var id string
				if err := json.Unmarshal(row["goalId"], &id); err != nil && len(row["goalId"]) > 0 {
					return err
				}
				if id != goalID {
					continue
				}
				cnt, tot, notes, err := goalProgressInTx(tx, user, goalID, r.from, r.at)
				if err != nil {
					return err
				}
				row["count"], _ = json.Marshal(cnt)
				row["amount"], _ = json.Marshal(tot)
				row["notes"], _ = json.Marshal(notes)
			}
			updatedJSON, err := json.Marshal(parsed)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE reports SET rows_json=? WHERE user_id=? AND id=?", string(updatedJSON), user, r.id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Goals(user string) ([]Goal, error) {
	rows, err := s.db.Query(`SELECT g.id,g.title,g.step,g.unit,g.created_at,COALESCE(a.proposal_id,''),
		COALESCE(a.memory_id,''),COALESCE(a.accepted_version,0),COALESCE(m.version,0),COALESCE(m.current_text,''),COALESCE(c.further_basis,'[]'),COALESCE(c.methods,'[]')
		FROM goals g LEFT JOIN goal_acceptances a ON a.goal_id=g.id LEFT JOIN goal_choices c ON c.goal_id=g.id
		LEFT JOIN memories m ON m.id=a.memory_id AND m.user_id=g.user_id
		WHERE g.user_id=? ORDER BY g.rowid DESC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		var g Goal
		var basis GoalBasis
		var extraBasis, methods string
		if err := rows.Scan(&g.ID, &g.Title, &g.Step, &g.Unit, &g.At, &g.ProposalID,
			&basis.ID, &basis.AcceptedVersion, &basis.CurrentVersion, &basis.CurrentText, &extraBasis, &methods); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(extraBasis), &g.ApprovedBasis); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(methods), &g.ApprovedMethods); err != nil {
			return nil, err
		}
		if basis.ID != "" {
			g.ApprovedBasis = append([]GoalBasisRef{{ID: basis.ID, Version: basis.AcceptedVersion}}, g.ApprovedBasis...)
			g.Basis = &basis
		}
		g.Entries = []GoalEntry{}
		goals = append(goals, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	for i := range goals {
		for _, ref := range goals[i].ApprovedBasis {
			current, e := resolveGoalBasisTx(tx, user, ref)
			if e != nil {
				tx.Rollback()
				return nil, e
			}
			goals[i].CurrentBasis = append(goals[i].CurrentBasis, current)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	for i := range goals {
		erows, err := s.db.Query("SELECT id,amount,note,created_at FROM goal_entries WHERE user_id=? AND goal_id=? ORDER BY rowid DESC", user, goals[i].ID)
		if err != nil {
			return nil, err
		}
		for erows.Next() {
			var e GoalEntry
			if err := erows.Scan(&e.ID, &e.Amount, &e.Note, &e.At); err != nil {
				erows.Close()
				return nil, err
			}
			goals[i].Entries = append(goals[i].Entries, e)
		}
		erows.Close()
	}
	return goals, nil
}

func (s *Store) WeeklyReport(user string, now time.Time) (Report, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	from := time.Date(now.Year(), now.Month(), now.Day()-6, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	at := now.UTC().Format(time.RFC3339)
	var report Report
	err := s.write(func(tx *sql.Tx) error {
		grows, err := tx.Query("SELECT id,title,step,unit FROM goals WHERE user_id=? ORDER BY rowid DESC", user)
		if err != nil {
			return err
		}
		var rows []ReportRow
		for grows.Next() {
			var row ReportRow
			if err := grows.Scan(&row.GoalID, &row.Title, &row.Step, &row.Unit); err != nil {
				grows.Close()
				return err
			}
			rows = append(rows, row)
		}
		grows.Close()
		hasProgress := false
		for i := range rows {
			cnt, tot, notes, err := goalProgressInTx(tx, user, rows[i].GoalID, from, at)
			if err != nil {
				return err
			}
			rows[i].Count, rows[i].Amount, rows[i].Notes = cnt, tot, notes
			if cnt > 0 {
				hasProgress = true
			}
		}
		if !hasProgress {
			return ErrInvalid
		}
		rowsJSON, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		report = Report{ID: opaqueID(18), At: at, From: from, Rows: rows}
		_, err = tx.Exec("INSERT INTO reports(id,user_id,from_at,created_at,rows_json) VALUES(?,?,?,?,?)",
			report.ID, user, report.From, report.At, string(rowsJSON))
		return err
	})
	return report, err
}

func (s *Store) Reports(user string) ([]Report, error) {
	rows, err := s.db.Query("SELECT id,from_at,created_at,rows_json FROM reports WHERE user_id=? ORDER BY rowid DESC", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		var r Report
		var raw string
		if err := rows.Scan(&r.ID, &r.From, &r.At, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r.Rows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

