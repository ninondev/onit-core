package main

// R40：自部署的持续目标复盘。
//
// 这一层只做三件事：把认证 owner 的目标、有效进展、明确选中的事实/来源和
// 确属该目标的可读产物整理成当前快照；复用已有的三步 Go 执行核
// （grPrepare/grExecute/grCompleteSave）在服务端进程里逐步落库；把四种
// 反馈和整项暂停变成以后是否继续工作的真实状态。
//
// 它不读写 Android 档案，不复制第二套步骤 switch，不调用模型，不写长期
// 目标完成状态。任务真源在 goal_review_works，结果历史在
// goal_review_results，反馈原文在 goal_review_feedback。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const goalReviewSchema = `
CREATE TABLE IF NOT EXISTS goal_review_follows (
	user_id TEXT NOT NULL REFERENCES users(id),
	goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
	enabled INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	request TEXT NOT NULL,
	selected TEXT NOT NULL DEFAULT '[]',
	timezone TEXT NOT NULL DEFAULT '',
	control_revision INTEGER NOT NULL DEFAULT 1,
	last_request_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY(user_id,goal_id)
);
CREATE TABLE IF NOT EXISTS goal_review_works (
	user_id TEXT NOT NULL REFERENCES users(id),
	goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
	work_id TEXT NOT NULL,
	digest TEXT NOT NULL,
	parts_json TEXT NOT NULL DEFAULT '{}',
	review_json TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	blocked_reason TEXT NOT NULL DEFAULT '',
	change_note TEXT NOT NULL DEFAULT '',
	result_id TEXT NOT NULL DEFAULT '',
	result_revision INTEGER NOT NULL DEFAULT 0,
	attention_revision INTEGER NOT NULL DEFAULT 0,
	seen_attention_revision INTEGER NOT NULL DEFAULT 0,
	feedback_json TEXT NOT NULL DEFAULT '',
	last_body_hash TEXT NOT NULL DEFAULT '',
	control_revision INTEGER NOT NULL DEFAULT 0,
	revision INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY(user_id,goal_id)
);
CREATE TABLE IF NOT EXISTS goal_review_results (
	user_id TEXT NOT NULL REFERENCES users(id),
	goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
	work_id TEXT NOT NULL,
	result_id TEXT NOT NULL,
	digest TEXT NOT NULL,
	body TEXT NOT NULL,
	result_json TEXT NOT NULL,
	invalidated INTEGER NOT NULL DEFAULT 0,
	invalidated_reason TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	PRIMARY KEY(user_id,goal_id,result_id)
);
CREATE TABLE IF NOT EXISTS goal_review_feedback (
	user_id TEXT NOT NULL REFERENCES users(id),
	goal_id TEXT NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
	request_id TEXT NOT NULL,
	action TEXT NOT NULL,
	work_id TEXT NOT NULL,
	result_id TEXT NOT NULL,
	result_revision INTEGER NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	due_at TEXT NOT NULL DEFAULT '',
	timezone TEXT NOT NULL DEFAULT '',
	body_hash TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	PRIMARY KEY(user_id,goal_id,request_id)
);
CREATE INDEX IF NOT EXISTS goal_review_results_goal ON goal_review_results(user_id,goal_id,created_at);
CREATE INDEX IF NOT EXISTS goal_review_feedback_goal ON goal_review_feedback(user_id,goal_id,created_at);`

const (
	grFollowTodo    = "todo"
	grFollowWaiting = "waiting"
	grFollowLater   = "later"
	grFollowHandled = "handled"

	grWorkBlocked = "blocked"

	grMaxFollowNote     = 500
	grMaxFollowRequest  = 4000
	grMaxFollowSelected = 40
	grMaxResultHistory  = 5
	grMaxTickSteps      = 6
)

var (
	goalReviewHookMu sync.Mutex
	goalReviewHookFn func(string) error
)

// goalReviewHook 是测试用的提交窗口：after-snapshot 在读快照后、写事务外；
// before-commit 在重读 CAS 之后、写入之前；save-readback 在提交成功之后。
// 生产路径为空。after-snapshot 里可以从另一 goroutine 写；before-commit
// 仍持有写锁，不能再从另一 goroutine 走 Store.write，否则 MaxOpenConns(1) 会死锁。
func goalReviewHook(stage string) error {
	goalReviewHookMu.Lock()
	fn := goalReviewHookFn
	goalReviewHookMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(stage)
}

func setGoalReviewHook(fn func(string) error) {
	goalReviewHookMu.Lock()
	goalReviewHookFn = fn
	goalReviewHookMu.Unlock()
}

type goalReviewFollow struct {
	UserID          string
	GoalID          string
	Enabled         bool
	Deleted         bool
	Request         string
	Selected        []preparationRef
	Timezone        string
	ControlRevision int
	LastRequestID   string
	CreatedAt       string
	UpdatedAt       string
}

type goalReviewWork struct {
	UserID                string
	GoalID                string
	WorkID                string
	Digest                string
	PartsJSON             string
	ReviewJSON            string
	Status                string
	BlockedReason         string
	ChangeNote            string
	ResultID              string
	ResultRevision        int
	AttentionRevision     int
	SeenAttentionRevision int
	FeedbackJSON          string
	LastBodyHash          string
	ControlRevision       int
	ExpectedControl       int
	Revision              int
	CreatedAt             string
	UpdatedAt             string
}

type goalReviewResult struct {
	ResultID          string
	WorkID            string
	Digest            string
	Body              string
	ResultJSON        string
	Invalidated       bool
	InvalidatedReason string
	CreatedAt         string
}

type goalReviewFeedback struct {
	RequestID      string
	Action         string
	WorkID         string
	ResultID       string
	ResultRevision int
	Note           string
	DueAt          string
	Timezone       string
	BodyHash       string
	Presented      bool
	CreatedAt      string
}

type goalReviewMaterialPart struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Version  int    `json:"version,omitempty"`
	TextHash string `json:"textHash,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
}

type goalReviewEntryPart struct {
	ID     string `json:"id"`
	Amount string `json:"amount"`
	Note   string `json:"note"`
}

type goalReviewArtifactPart struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	At    string `json:"at"`
	Hash  string `json:"hash,omitempty"`
}

type goalReviewParts struct {
	GoalID    string                   `json:"goalId"`
	Title     string                   `json:"title"`
	Step      string                   `json:"step"`
	Unit      string                   `json:"unit"`
	Request   string                   `json:"request"`
	Selected  []string                 `json:"selected"`
	Materials []goalReviewMaterialPart `json:"materials"`
	Entries   []goalReviewEntryPart    `json:"entries"`
	Artifacts []goalReviewArtifactPart `json:"artifacts"`
}

type goalReviewSnapshot struct {
	Follow  goalReviewFollow
	Goal    object
	D       *dossierRows
	Parts   goalReviewParts
	Digest  string
	Blocked string
}

type goalReviewFollowInput struct {
	RequestID               string           `json:"requestId"`
	ExpectedControlRevision *int             `json:"expectedControlRevision"`
	Enabled                 bool             `json:"enabled"`
	Timezone                string           `json:"timezone"`
	Request                 string           `json:"request"`
	Selected                []preparationRef `json:"selected"`
}

type goalReviewFeedbackInput struct {
	RequestID      string `json:"requestId"`
	Action         string `json:"action"`
	ResultID       string `json:"resultId"`
	ResultRevision int    `json:"resultRevision"`
	Note           string `json:"note"`
	DueAt          string `json:"dueAt"`
	Timezone       string `json:"timezone"`
}

// goalReviewNow is the production clock. ADVISER_TEST_NOW is a test-only
// injection used by the isolated acceptance harness; it is never set by the
// product and does not change stored timestamps.
func goalReviewNow() time.Time {
	if raw := strings.TrimSpace(os.Getenv("ADVISER_TEST_NOW")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

func goalReviewDigest(user string, parts goalReviewParts) string {
	sum := sha256.Sum256([]byte(user + "\n" + encode(parts)))
	return hex.EncodeToString(sum[:])
}

func goalReviewTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func goalReviewRefKey(ref preparationRef) string { return ref.Kind + ":" + ref.ID }

func goalReviewRefsAny(refs []preparationRef) []any {
	out := make([]any, len(refs))
	for i := range refs {
		out[i] = object{"kind": refs[i].Kind, "id": refs[i].ID}
	}
	return out
}

func goalReviewSelectedJSON(refs []preparationRef) string { return encode(refs) }

func sameGoalReviewRefs(a, b []preparationRef) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, ref := range a {
		set[ref.Kind+":"+ref.ID] = true
	}
	for _, ref := range b {
		if !set[ref.Kind+":"+ref.ID] {
			return false
		}
	}
	return true
}

func goalReviewParseSelected(raw string) []preparationRef {
	var refs []preparationRef
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	return refs
}

func goalReviewCleanSelected(selected []preparationRef) ([]preparationRef, error) {
	if len(selected) > grMaxFollowSelected {
		return nil, ErrInvalid
	}
	clean := make([]preparationRef, 0, len(selected))
	seen := map[string]bool{}
	for _, ref := range selected {
		kind := strings.TrimSpace(ref.Kind)
		id := strings.TrimSpace(ref.ID)
		if kind == "" {
			kind = "fact"
		}
		if (kind != "fact" && kind != "source") || id == "" || len(id) > 120 {
			return nil, ErrInvalid
		}
		key := kind + ":" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, preparationRef{Kind: kind, ID: id})
	}
	return clean, nil
}

func readGoalReviewFollowTx(tx *sql.Tx, user, goalID string) (goalReviewFollow, error) {
	var f goalReviewFollow
	var enabled, deleted int
	var selected string
	err := tx.QueryRow(`SELECT user_id,goal_id,enabled,deleted,request,selected,timezone,control_revision,last_request_id,created_at,updated_at
		FROM goal_review_follows WHERE user_id=? AND goal_id=?`, user, goalID).
		Scan(&f.UserID, &f.GoalID, &enabled, &deleted, &f.Request, &selected, &f.Timezone, &f.ControlRevision, &f.LastRequestID, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return f, missing(err)
	}
	f.Enabled = enabled != 0
	f.Deleted = deleted != 0
	f.Selected = goalReviewParseSelected(selected)
	return f, nil
}

func readGoalReviewWorkTx(tx *sql.Tx, user, goalID string) (*goalReviewWork, error) {
	var w goalReviewWork
	err := tx.QueryRow(`SELECT user_id,goal_id,work_id,digest,parts_json,review_json,status,blocked_reason,change_note,
		result_id,result_revision,attention_revision,seen_attention_revision,feedback_json,last_body_hash,control_revision,revision,created_at,updated_at
		FROM goal_review_works WHERE user_id=? AND goal_id=?`, user, goalID).
		Scan(&w.UserID, &w.GoalID, &w.WorkID, &w.Digest, &w.PartsJSON, &w.ReviewJSON, &w.Status, &w.BlockedReason, &w.ChangeNote,
			&w.ResultID, &w.ResultRevision, &w.AttentionRevision, &w.SeenAttentionRevision, &w.FeedbackJSON, &w.LastBodyHash, &w.ControlRevision, &w.Revision, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.ExpectedControl = w.ControlRevision
	return &w, nil
}

func insertGoalReviewWorkTx(tx *sql.Tx, w *goalReviewWork) error {
	w.ExpectedControl = w.ControlRevision
	_, err := tx.Exec(`INSERT INTO goal_review_works
		(user_id,goal_id,work_id,digest,parts_json,review_json,status,blocked_reason,change_note,result_id,result_revision,
		 attention_revision,seen_attention_revision,feedback_json,last_body_hash,control_revision,revision,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.UserID, w.GoalID, w.WorkID, w.Digest, w.PartsJSON, w.ReviewJSON, w.Status, w.BlockedReason, w.ChangeNote,
		w.ResultID, w.ResultRevision, w.AttentionRevision, w.SeenAttentionRevision, w.FeedbackJSON, w.LastBodyHash, w.ControlRevision,
		w.Revision, w.CreatedAt, w.UpdatedAt)
	return err
}

func replaceGoalReviewWorkTx(tx *sql.Tx, w *goalReviewWork) error {
	if _, err := tx.Exec(`DELETE FROM goal_review_works WHERE user_id=? AND goal_id=?`, w.UserID, w.GoalID); err != nil {
		return err
	}
	return insertGoalReviewWorkTx(tx, w)
}

// updateGoalReviewWorkTx is a compare-and-swap on the work revision and the
// policy revision the worker read. A pause, revocation or feedback that lands
// first changes one of them, so the old worker's write affects zero rows.
func updateGoalReviewWorkTx(tx *sql.Tx, w *goalReviewWork, expectedRevision, expectedControl int) error {
	res, err := tx.Exec(`UPDATE goal_review_works SET digest=?,parts_json=?,review_json=?,status=?,blocked_reason=?,change_note=?,
		result_id=?,result_revision=?,attention_revision=?,seen_attention_revision=?,feedback_json=?,last_body_hash=?,control_revision=?,
		revision=revision+1,updated_at=?
		WHERE user_id=? AND goal_id=? AND work_id=? AND revision=? AND control_revision=?`,
		w.Digest, w.PartsJSON, w.ReviewJSON, w.Status, w.BlockedReason, w.ChangeNote,
		w.ResultID, w.ResultRevision, w.AttentionRevision, w.SeenAttentionRevision, w.FeedbackJSON, w.LastBodyHash, w.ControlRevision,
		w.UpdatedAt, w.UserID, w.GoalID, w.WorkID, expectedRevision, expectedControl)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			return ErrConflict
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	w.Revision = expectedRevision + 1
	w.ExpectedControl = w.ControlRevision
	return nil
}

func readGoalReviewResultsTx(tx *sql.Tx, user, goalID string, limit int) ([]goalReviewResult, error) {
	if limit <= 0 {
		limit = grMaxResultHistory
	}
	rows, err := tx.Query(`SELECT result_id,work_id,digest,body,result_json,invalidated,invalidated_reason,created_at
		FROM goal_review_results WHERE user_id=? AND goal_id=? ORDER BY created_at DESC, result_id DESC LIMIT ?`, user, goalID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []goalReviewResult{}
	for rows.Next() {
		var r goalReviewResult
		var invalidated int
		if err := rows.Scan(&r.ResultID, &r.WorkID, &r.Digest, &r.Body, &r.ResultJSON, &invalidated, &r.InvalidatedReason, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Invalidated = invalidated != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func readGoalReviewFeedbackTx(tx *sql.Tx, user, goalID, requestID string) (*goalReviewFeedback, error) {
	var f goalReviewFeedback
	err := tx.QueryRow(`SELECT request_id,action,work_id,result_id,result_revision,note,due_at,timezone,body_hash,created_at
		FROM goal_review_feedback WHERE user_id=? AND goal_id=? AND request_id=?`, user, goalID, requestID).
		Scan(&f.RequestID, &f.Action, &f.WorkID, &f.ResultID, &f.ResultRevision, &f.Note, &f.DueAt, &f.Timezone, &f.BodyHash, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func decodeGoalReviewFeedback(raw string) goalReviewFeedback {
	var f goalReviewFeedback
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &f)
	}
	return f
}

const grStaleInputReason = "输入已变化，旧成果已失效。"

func invalidateGoalReviewResultTx(tx *sql.Tx, user, goalID, workID, reason string) error {
	if workID == "" {
		return nil
	}
	_, err := tx.Exec(`UPDATE goal_review_results SET invalidated=1,invalidated_reason=?,body='',result_json='{}'
		WHERE user_id=? AND goal_id=? AND work_id=? AND invalidated=0`, reason, user, goalID, workID)
	return err
}

func stripGoalReviewWorkResult(w *goalReviewWork, reason string) {
	if w == nil {
		return
	}
	w.ResultID = ""
	if w.ReviewJSON == "" {
		return
	}
	review, err := parseObject([]byte(w.ReviewJSON))
	if err != nil || review == nil {
		return
	}
	review["result"] = nil
	review["resultId"] = ""
	if reason != "" {
		review["staleReason"] = reason
	}
	w.ReviewJSON = encode(review)
}

func goalReviewWorkHasLiveBody(w *goalReviewWork) bool {
	if w == nil {
		return false
	}
	if strings.TrimSpace(w.ResultID) != "" {
		return true
	}
	if w.ReviewJSON == "" {
		return false
	}
	review, err := parseObject([]byte(w.ReviewJSON))
	if err != nil || review == nil {
		return false
	}
	result, _ := review["result"].(object)
	return result != nil && str(result, "body") != ""
}

// maintainGoalReviewCurrentnessTx clears invalidated current prose even when
// waiting or paused forbids a new generation. Digest is left unchanged so an
// explicit continue can still see that input moved. Repeat GET/tick is a
// no-op once the live body is gone, so it does not bump revisions.
func maintainGoalReviewCurrentnessTx(tx *sql.Tx, user, goalID string, w *goalReviewWork, snap goalReviewSnapshot) error {
	if w == nil || w.Digest == snap.Digest {
		return nil
	}
	var live int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM goal_review_results WHERE user_id=? AND goal_id=? AND work_id=? AND invalidated=0`,
		user, goalID, w.WorkID).Scan(&live); err != nil {
		return err
	}
	if live == 0 && !goalReviewWorkHasLiveBody(w) {
		return nil
	}
	if err := invalidateGoalReviewResultTx(tx, user, goalID, w.WorkID, grStaleInputReason); err != nil {
		return err
	}
	stripGoalReviewWorkResult(w, grStaleInputReason)
	w.UpdatedAt = grNow()
	if _, err := tx.Exec(`UPDATE goal_review_works SET review_json=?,result_id='',revision=revision+1,updated_at=?
		WHERE user_id=? AND goal_id=? AND work_id=?`,
		w.ReviewJSON, w.UpdatedAt, user, goalID, w.WorkID); err != nil {
		return err
	}
	w.Revision++
	return nil
}

func goalReviewSnapshotTx(tx *sql.Tx, user, goalID string) (goalReviewSnapshot, error) {
	var snap goalReviewSnapshot
	f, err := readGoalReviewFollowTx(tx, user, goalID)
	if err != nil {
		return snap, err
	}
	if f.Deleted {
		return snap, ErrNotFound
	}
	snap.Follow = f
	var title, step, unit, at string
	if err := tx.QueryRow(`SELECT title,step,unit,created_at FROM goals WHERE user_id=? AND id=?`, user, goalID).
		Scan(&title, &step, &unit, &at); err != nil {
		return snap, missing(err)
	}
	entries := []object{}
	entryParts := []goalReviewEntryPart{}
	erows, err := tx.Query(`SELECT id,amount,note,created_at FROM goal_entries WHERE user_id=? AND goal_id=? ORDER BY rowid DESC`, user, goalID)
	if err != nil {
		return snap, err
	}
	for erows.Next() {
		var id, note, createdAt string
		var amount float64
		if err := erows.Scan(&id, &amount, &note, &createdAt); err != nil {
			erows.Close()
			return snap, err
		}
		entries = append(entries, object{"id": id, "amount": amount, "note": note, "at": createdAt})
		entryParts = append(entryParts, goalReviewEntryPart{ID: id, Amount: grAmountString(amount), Note: note})
	}
	if err := erows.Err(); err != nil {
		erows.Close()
		return snap, err
	}
	erows.Close()
	sort.Slice(entryParts, func(i, j int) bool { return entryParts[i].ID < entryParts[j].ID })

	set, err := resolveSelectedMaterialsTx(tx, user, f.Selected)
	if err != nil {
		return snap, err
	}
	d := &dossierRows{
		fields:  object{"artifacts": []any{}},
		rows:    map[string][]object{},
		sources: map[string]object{},
		entries: map[string]string{},
	}
	d.rows["facts"] = []object{}
	for _, m := range set.Facts {
		item := object{
			"id": m.ID, "sourceId": m.SourceID, "text": m.Text, "quote": m.Quote,
			"version": m.Version, "at": "", "confirmed": m.Confirmed, "attribution": m.Attribution,
		}
		if len(m.Evidence) > 0 {
			ev := make([]any, 0, len(m.Evidence))
			for _, sp := range m.Evidence {
				ev = append(ev, object{"sourceId": sp.SourceID, "quote": sp.Quote})
			}
			item["evidence"] = ev
		}
		d.rows["facts"] = append(d.rows["facts"], item)
	}
	d.rows["sources"] = []object{}
	for _, src := range append(append([]Source{}, set.Sources...), set.FactSources...) {
		o := object{"id": src.ID, "title": src.Title, "kind": src.Kind, "origin": src.Origin, "text": src.Text, "at": src.At}
		d.sources[src.ID] = o
		d.rows["sources"] = append(d.rows["sources"], o)
	}
	d.rows["discoveries"] = []object{}
	d.rows["goals"] = []object{}
	d.rows["entries"] = entries
	d.rows["proposals"] = []object{}
	d.rows["dismissed"] = []object{}
	d.rows["conversation"] = []object{}
	for _, entry := range entries {
		d.entries[str(entry, "id")] = goalID
	}
	reports := []object{}
	rrows, err := tx.Query(`SELECT id,from_at,created_at,rows_json FROM reports WHERE user_id=? ORDER BY rowid DESC`, user)
	if err != nil {
		return snap, err
	}
	for rrows.Next() {
		var id, from, createdAt, raw string
		if err := rrows.Scan(&id, &from, &createdAt, &raw); err != nil {
			rrows.Close()
			return snap, err
		}
		var rows []ReportRow
		if err := json.Unmarshal([]byte(raw), &rows); err != nil {
			rrows.Close()
			return snap, err
		}
		goalRows := []any{}
		for _, row := range rows {
			if row.GoalID != goalID {
				continue
			}
			goalRows = append(goalRows, object{
				"goalId": row.GoalID, "title": row.Title, "step": row.Step, "unit": row.Unit,
				"count": row.Count, "amount": row.Amount, "notes": grAnyStrings(row.Notes),
			})
		}
		if len(goalRows) == 0 {
			continue
		}
		reports = append(reports, object{"id": id, "at": createdAt, "from": from, "rows": goalRows})
	}
	if err := rrows.Err(); err != nil {
		rrows.Close()
		return snap, err
	}
	rrows.Close()
	d.rows["reports"] = reports
	cards, prepHash, err := currentPreparationCardsTx(tx, user, goalID)
	if err != nil {
		return snap, err
	}
	d.fields["artifacts"] = cards

	goal := object{
		"id": goalID, "title": title, "step": step, "unit": unit, "at": at,
		"entries": grAny(entries),
		"brief": object{
			"request":      f.Request,
			"materialRefs": goalReviewRefsAny(f.Selected),
		},
	}
	snap.Goal = goal
	snap.D = d

	parts := goalReviewParts{
		GoalID: goalID, Title: title, Step: step, Unit: unit, Request: f.Request,
		Selected: []string{}, Materials: []goalReviewMaterialPart{},
		Entries: entryParts, Artifacts: []goalReviewArtifactPart{},
	}
	missing := map[string]bool{}
	for _, ref := range set.Missing {
		missing[goalReviewRefKey(ref)] = true
	}
	for _, ref := range f.Selected {
		key := goalReviewRefKey(ref)
		part := goalReviewMaterialPart{Kind: ref.Kind, ID: ref.ID}
		if missing[key] {
			part.Missing = true
		} else if material, ok := grMaterial(d, object{"kind": ref.Kind, "id": ref.ID}); ok {
			part.Version = mustInt(material["version"])
			part.TextHash = goalReviewTextHash(str(material, "text"))
		} else {
			part.Missing = true
		}
		parts.Selected = append(parts.Selected, key)
		parts.Materials = append(parts.Materials, part)
	}
	sort.Strings(parts.Selected)
	sort.Slice(parts.Materials, func(i, j int) bool {
		return parts.Materials[i].Kind+":"+parts.Materials[i].ID < parts.Materials[j].Kind+":"+parts.Materials[j].ID
	})
	reportHash := map[string]string{}
	for _, report := range reports {
		reportHash[str(report, "id")] = goalReviewTextHash(encode(report["rows"]))
	}
	for _, artifact := range grReadableArtifacts(d, goal) {
		id := str(artifact, "id")
		hash := reportHash[id]
		if hash == "" {
			hash = prepHash[id]
		}
		parts.Artifacts = append(parts.Artifacts, goalReviewArtifactPart{
			ID: id, Kind: str(artifact, "kind"), Title: str(artifact, "title"),
			At: str(artifact, "at"), Hash: hash,
		})
	}
	sort.Slice(parts.Artifacts, func(i, j int) bool { return parts.Artifacts[i].ID < parts.Artifacts[j].ID })
	snap.Parts = parts
	snap.Digest = goalReviewDigest(user, parts)
	switch {
	case len(f.Selected) == 0:
		snap.Blocked = "没有选中材料；持续复盘不会在没有明确来源时工作。"
	case len(set.Missing) > 0:
		snap.Blocked = "选中的材料已经撤回、来源缺失或引文不再一致；请重新选择后再继续。"
	case len(set.FactIssues) > 0:
		snap.Blocked = "选中的事实引文与来源不再一致；请重新核对后再继续。"
	}
	return snap, nil
}

func goalReviewChangeNote(prev, next goalReviewParts) string {
	changes := []string{}
	if prev.Request != next.Request {
		changes = append(changes, "请求已更新")
	}
	if prev.Title != next.Title || prev.Step != next.Step || prev.Unit != next.Unit {
		changes = append(changes, "目标本身有更新")
	}
	prevSel, nextSel := map[string]bool{}, map[string]bool{}
	for _, key := range prev.Selected {
		prevSel[key] = true
	}
	for _, key := range next.Selected {
		nextSel[key] = true
	}
	added, removed := 0, 0
	for key := range nextSel {
		if !prevSel[key] {
			added++
		}
	}
	for key := range prevSel {
		if !nextSel[key] {
			removed++
		}
	}
	if added > 0 || removed > 0 {
		changes = append(changes, fmt.Sprintf("选中的材料有增减（新增 %d，移除 %d）", added, removed))
	}
	prevMat, nextMat := map[string]goalReviewMaterialPart{}, map[string]goalReviewMaterialPart{}
	for _, m := range prev.Materials {
		prevMat[m.Kind+":"+m.ID] = m
	}
	for _, m := range next.Materials {
		nextMat[m.Kind+":"+m.ID] = m
	}
	changed := 0
	for key, m := range nextMat {
		if old, ok := prevMat[key]; ok && (old.Version != m.Version || old.TextHash != m.TextHash || old.Missing != m.Missing) {
			changed++
		}
	}
	if changed > 0 {
		changes = append(changes, fmt.Sprintf("选中的材料内容或版本有更新（%d 份）", changed))
	}
	prevEnt, nextEnt := map[string]goalReviewEntryPart{}, map[string]goalReviewEntryPart{}
	for _, e := range prev.Entries {
		prevEnt[e.ID] = e
	}
	for _, e := range next.Entries {
		nextEnt[e.ID] = e
	}
	addedE, removedE, changedE := 0, 0, 0
	for id, e := range nextEnt {
		old, ok := prevEnt[id]
		if !ok {
			addedE++
		} else if old.Amount != e.Amount || old.Note != e.Note {
			changedE++
		}
	}
	for id := range prevEnt {
		if _, ok := nextEnt[id]; !ok {
			removedE++
		}
	}
	// 身份改名没有公开 API；独立验收用夹具改 ID 且金额/说明不变。
	// 按金额+说明把未匹配项 1:1 配掉，不把纯改名写成新增+删除。
	type entryKey struct{ Amount, Note string }
	unmatchedPrev := map[entryKey]int{}
	for id, e := range prevEnt {
		if _, ok := nextEnt[id]; !ok {
			unmatchedPrev[entryKey{e.Amount, e.Note}]++
		}
	}
	paired := 0
	for id, e := range nextEnt {
		if _, ok := prevEnt[id]; ok {
			continue
		}
		key := entryKey{e.Amount, e.Note}
		if unmatchedPrev[key] > 0 {
			unmatchedPrev[key]--
			paired++
		}
	}
	addedE -= paired
	removedE -= paired
	if addedE+removedE+changedE > 0 {
		changes = append(changes, fmt.Sprintf("目标的进展记录有更新（新增 %d，更正 %d，删除 %d）", addedE, changedE, removedE))
	}
	prevArt, nextArt := map[string]goalReviewArtifactPart{}, map[string]goalReviewArtifactPart{}
	for _, a := range prev.Artifacts {
		prevArt[a.ID] = a
	}
	for _, a := range next.Artifacts {
		nextArt[a.ID] = a
	}
	addedA, removedA, changedA := 0, 0, 0
	for id, a := range nextArt {
		old, ok := prevArt[id]
		if !ok {
			addedA++
		} else if old.Hash != a.Hash || old.Title != a.Title || old.At != a.At {
			changedA++
		}
	}
	for id := range prevArt {
		if _, ok := nextArt[id]; !ok {
			removedA++
		}
	}
	if addedA+removedA+changedA > 0 {
		changes = append(changes, fmt.Sprintf("可读产物有更新（新增 %d，变更 %d，移除 %d）", addedA, changedA, removedA))
	}
	if len(changes) == 0 {
		return ""
	}
	return strings.Join(changes, "；")
}

func goalReviewWorkObject(w *goalReviewWork) object {
	if w == nil {
		return nil
	}
	out := object{
		"workId": w.WorkID, "digest": w.Digest, "status": w.Status,
		"blockedReason": w.BlockedReason, "changeNote": w.ChangeNote,
		"resultId": w.ResultID, "resultRevision": w.ResultRevision, "attentionRevision": w.AttentionRevision,
		"seenAttentionRevision": w.SeenAttentionRevision,
		"unread":                w.AttentionRevision > w.SeenAttentionRevision,
		"updatedAt":             w.UpdatedAt,
	}
	if w.ReviewJSON != "" {
		if review, err := parseObject([]byte(w.ReviewJSON)); err == nil {
			out["steps"] = review["steps"]
			out["attempts"] = review["attempts"]
			out["error"] = review["error"]
			out["staleReason"] = review["staleReason"]
			out["reviewId"] = review["id"]
			if result, _ := review["result"].(object); result != nil {
				out["result"] = result
			}
		}
	}
	if fb := decodeGoalReviewFeedback(w.FeedbackJSON); fb.Action != "" {
		out["feedback"] = object{
			"action": fb.Action, "note": fb.Note, "dueAt": fb.DueAt, "timezone": fb.Timezone,
			"resultId": fb.ResultID, "resultRevision": fb.ResultRevision, "presented": fb.Presented,
		}
	}
	return out
}

func goalReviewFeedTx(tx *sql.Tx, user, goalID string, origin string) (object, error) {
	f, err := readGoalReviewFollowTx(tx, user, goalID)
	if err != nil {
		return nil, err
	}
	if f.Deleted {
		return nil, ErrNotFound
	}
	snap, err := goalReviewSnapshotTx(tx, user, goalID)
	if err != nil {
		return nil, err
	}
	w, err := readGoalReviewWorkTx(tx, user, goalID)
	if err != nil {
		return nil, err
	}
	if err := maintainGoalReviewCurrentnessTx(tx, user, goalID, w, snap); err != nil {
		return nil, err
	}
	history, err := readGoalReviewResultsTx(tx, user, goalID, grMaxResultHistory)
	if err != nil {
		return nil, err
	}
	historyObjects := make([]any, 0, len(history))
	for _, r := range history {
		historyObjects = append(historyObjects, object{
			"id": r.ResultID, "workId": r.WorkID, "body": r.Body, "createdAt": r.CreatedAt,
			"invalidated": r.Invalidated, "invalidatedReason": r.InvalidatedReason,
		})
	}
	feed := object{
		"goalId": f.GoalID, "enabled": f.Enabled, "request": f.Request,
		"selected": goalReviewRefsAny(f.Selected), "timezone": f.Timezone,
		"controlRevision": f.ControlRevision, "history": historyObjects,
		"origin": origin, "account": preparationAccount(user),
	}
	if w != nil {
		feed["work"] = goalReviewWorkObject(w)
	} else {
		feed["work"] = nil
	}
	return feed, nil
}

func (s *Store) GoalReviewFeed(user, goalID, origin string) (object, error) {
	var feed object
	err := s.write(func(tx *sql.Tx) error {
		var err error
		feed, err = goalReviewFeedTx(tx, user, goalID, origin)
		return err
	})
	return feed, err
}

func (s *Store) GoalReviewFeeds(user, origin string) ([]object, error) {
	var out []object
	err := s.write(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT goal_id FROM goal_review_follows WHERE user_id=? AND deleted=0 ORDER BY updated_at DESC, goal_id`, user)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			feed, err := goalReviewFeedTx(tx, user, id, origin)
			if err != nil {
				return err
			}
			out = append(out, feed)
		}
		return nil
	})
	return out, err
}

func (s *Store) SaveGoalReviewFollow(user, goalID string, in goalReviewFollowInput) (object, error) {
	goalID = strings.TrimSpace(goalID)
	requestID := strings.TrimSpace(in.RequestID)
	request := strings.TrimSpace(in.Request)
	timezone := strings.TrimSpace(in.Timezone)
	if goalID == "" || len(goalID) > 120 || requestID == "" || len(requestID) > 120 {
		return nil, ErrInvalid
	}
	if utf8.RuneCountInString(request) > grMaxFollowRequest {
		return nil, ErrInvalid
	}
	clean, err := goalReviewCleanSelected(in.Selected)
	if err != nil {
		return nil, err
	}
	if timezone != "" {
		if len(timezone) > 80 || timezone == "Local" {
			return nil, ErrInvalid
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return nil, ErrInvalid
		}
	}
	var feed object
	err = s.write(func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM goals WHERE user_id=? AND id=?`, user, goalID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		old, oldErr := readGoalReviewFollowTx(tx, user, goalID)
		existing := oldErr == nil
		if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
			return oldErr
		}
		now := grNow()
		if existing && old.LastRequestID == requestID {
			if old.Deleted || old.Enabled != in.Enabled || old.Request != request || old.Timezone != timezone || !sameGoalReviewRefs(old.Selected, clean) {
				return ErrConflict
			}
			var err error
			feed, err = goalReviewFeedTx(tx, user, goalID, "")
			return err
		}
		if existing && in.ExpectedControlRevision != nil && *in.ExpectedControlRevision != old.ControlRevision {
			return ErrConflict
		}
		if !existing && in.ExpectedControlRevision != nil && *in.ExpectedControlRevision != 0 {
			return ErrConflict
		}
		if !existing && !in.Enabled {
			return ErrInvalid
		}
		if !existing && (request == "" || len(clean) == 0) {
			return ErrInvalid
		}
		if in.Enabled && !existing && len(clean) == 0 {
			return ErrInvalid
		}
		// 首次开启必须指向真实存在的材料；以后撤权或更正时允许保存，
		// 由 tick 把缺料变成稳定的 blocked 状态，而不是用 invalid_input
		// 阻止用户撤回。
		if !existing && in.Enabled {
			for _, ref := range clean {
				table := "memories"
				if ref.Kind == "source" {
					table = "sources"
				}
				var n int
				if err := tx.QueryRow("SELECT COUNT(1) FROM "+table+" WHERE user_id=? AND id=?", user, ref.ID).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return ErrNotFound
				}
			}
		}
		revision := 1
		createdAt := now
		if existing {
			revision = old.ControlRevision + 1
			createdAt = old.CreatedAt
			if _, err := tx.Exec(`UPDATE goal_review_follows SET enabled=?,deleted=0,request=?,selected=?,timezone=?,
				control_revision=?,last_request_id=?,updated_at=? WHERE user_id=? AND goal_id=?`,
				boolInt(in.Enabled), request, goalReviewSelectedJSON(clean), timezone, revision, requestID, now, user, goalID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`INSERT INTO goal_review_follows
				(user_id,goal_id,enabled,deleted,request,selected,timezone,control_revision,last_request_id,created_at,updated_at)
				VALUES(?,?,?,0,?,?,?,?,?,?,?)`,
				user, goalID, boolInt(in.Enabled), request, goalReviewSelectedJSON(clean), timezone, revision, requestID, createdAt, now); err != nil {
				return err
			}
		}
		// 输入真的变了就立刻让旧成果失效；只是暂停/恢复但内容相同，则保留
		// 仍有效的成果，并把当前任务的控制版本推进到新版本，避免旧 worker
		// 继续写。
		snap, err := goalReviewSnapshotTx(tx, user, goalID)
		if err != nil {
			return err
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w != nil {
			if err := maintainGoalReviewCurrentnessTx(tx, user, goalID, w, snap); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE goal_review_works SET control_revision=?,updated_at=? WHERE user_id=? AND goal_id=? AND work_id=?`,
				revision, now, user, goalID, w.WorkID); err != nil {
				return err
			}
		}
		feed, err = goalReviewFeedTx(tx, user, goalID, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	if in.Enabled {
		if _, advErr := s.advanceGoalReview(user, goalID, grMaxTickSteps, goalReviewNow()); advErr != nil && !errors.Is(advErr, ErrConflict) {
			return nil, advErr
		}
		feed, err = s.GoalReviewFeed(user, goalID, "")
	}
	return feed, err
}

func (s *Store) DeleteGoalReviewFollow(user, goalID, requestID string, expected *int) error {
	goalID = strings.TrimSpace(goalID)
	requestID = strings.TrimSpace(requestID)
	if goalID == "" || requestID == "" || len(requestID) > 120 {
		return ErrInvalid
	}
	return s.write(func(tx *sql.Tx) error {
		old, err := readGoalReviewFollowTx(tx, user, goalID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if old.Deleted {
			return nil
		}
		if expected != nil && *expected != old.ControlRevision {
			return ErrConflict
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w != nil {
			if err := invalidateGoalReviewResultTx(tx, user, goalID, w.WorkID, "持续复盘已关闭，旧成果已失效。"); err != nil {
				return err
			}
		}
		now := grNow()
		if _, err := tx.Exec(`UPDATE goal_review_follows SET enabled=0,deleted=1,control_revision=control_revision+1,last_request_id=?,updated_at=?
			WHERE user_id=? AND goal_id=?`, requestID, now, user, goalID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM goal_review_works WHERE user_id=? AND goal_id=?`, user, goalID)
		return err
	})
}

func (s *Store) GoalReviewFeedback(user, goalID string, in goalReviewFeedbackInput) (object, error) {
	goalID = strings.TrimSpace(goalID)
	requestID := strings.TrimSpace(in.RequestID)
	action := strings.TrimSpace(in.Action)
	note := strings.TrimSpace(in.Note)
	dueRaw := strings.TrimSpace(in.DueAt)
	timezone := strings.TrimSpace(in.Timezone)
	if goalID == "" || requestID == "" || len(requestID) > 120 || in.ResultID == "" || in.ResultRevision < 1 {
		return nil, ErrInvalid
	}
	if action != grFollowTodo && action != grFollowWaiting && action != grFollowLater && action != grFollowHandled {
		return nil, ErrInvalid
	}
	if utf8.RuneCountInString(note) > grMaxFollowNote {
		return nil, ErrInvalid
	}
	var dueUTC string
	var dueTime time.Time
	if action == grFollowWaiting {
		if note == "" || dueRaw != "" || timezone != "" {
			return nil, ErrInvalid
		}
	}
	if action == grFollowLater {
		if dueRaw == "" || timezone == "" || len(timezone) > 80 || timezone == "Local" {
			return nil, ErrInvalid
		}
		if _, err := time.LoadLocation(timezone); err != nil {
			return nil, ErrInvalid
		}
		t, err := time.Parse(time.RFC3339, dueRaw)
		if err != nil {
			return nil, ErrInvalid
		}
		dueTime = t.UTC()
		dueUTC = dueTime.Format(time.RFC3339)
	}
	if action != grFollowLater && (dueRaw != "" || timezone != "") {
		return nil, ErrInvalid
	}
	var feed object
	err := s.write(func(tx *sql.Tx) error {
		f, err := readGoalReviewFollowTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if f.Deleted {
			return ErrNotFound
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w == nil {
			return ErrNotFound
		}
		existing, err := readGoalReviewFeedbackTx(tx, user, goalID, requestID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Action != action || existing.ResultID != in.ResultID || existing.ResultRevision != in.ResultRevision ||
				existing.Note != note || existing.DueAt != dueUTC || existing.Timezone != timezone {
				return ErrConflict
			}
			feed, err = goalReviewFeedTx(tx, user, goalID, "")
			return err
		}
		if action == grFollowLater && !dueTime.After(goalReviewNow()) {
			return ErrInvalid
		}
		if w.Status != "completed" || w.ResultID != in.ResultID || w.ResultRevision != in.ResultRevision {
			return ErrConflict
		}
		resultBody := ""
		if err := tx.QueryRow(`SELECT body FROM goal_review_results WHERE user_id=? AND goal_id=? AND result_id=? AND invalidated=0`,
			user, goalID, in.ResultID).Scan(&resultBody); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConflict
			}
			return err
		}
		bodyHash := goalReviewTextHash(resultBody)
		now := grNow()
		if _, err := tx.Exec(`INSERT INTO goal_review_feedback
			(user_id,goal_id,request_id,action,work_id,result_id,result_revision,note,due_at,timezone,body_hash,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			user, goalID, requestID, action, w.WorkID, in.ResultID, in.ResultRevision, note, dueUTC, timezone, bodyHash, now); err != nil {
			return err
		}
		fb := goalReviewFeedback{
			RequestID: requestID, Action: action, WorkID: w.WorkID, ResultID: in.ResultID,
			ResultRevision: in.ResultRevision, Note: note, DueAt: dueUTC, Timezone: timezone,
			BodyHash: bodyHash, CreatedAt: now,
		}
		revision := f.ControlRevision + 1
		if _, err := tx.Exec(`UPDATE goal_review_follows SET control_revision=?,updated_at=? WHERE user_id=? AND goal_id=?`,
			revision, now, user, goalID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE goal_review_works SET feedback_json=?,control_revision=?,revision=revision+1,updated_at=?
			WHERE user_id=? AND goal_id=? AND work_id=?`, encode(fb), revision, now, user, goalID, w.WorkID); err != nil {
			return err
		}
		feed, err = goalReviewFeedTx(tx, user, goalID, "")
		return err
	})
	return feed, err
}

func (s *Store) ContinueGoalReview(user, goalID, requestID string, expected *int) (object, error) {
	goalID = strings.TrimSpace(goalID)
	requestID = strings.TrimSpace(requestID)
	if goalID == "" || requestID == "" || len(requestID) > 120 {
		return nil, ErrInvalid
	}
	err := s.write(func(tx *sql.Tx) error {
		f, err := readGoalReviewFollowTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if f.Deleted {
			return ErrNotFound
		}
		if expected != nil && *expected != f.ControlRevision {
			return ErrConflict
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w == nil {
			return ErrNotFound
		}
		fb := decodeGoalReviewFeedback(w.FeedbackJSON)
		if fb.Action != grFollowWaiting && fb.Action != grFollowLater {
			return ErrInvalid
		}
		now := grNow()
		revision := f.ControlRevision + 1
		if _, err := tx.Exec(`UPDATE goal_review_follows SET control_revision=?,last_request_id=?,updated_at=? WHERE user_id=? AND goal_id=?`,
			revision, requestID, now, user, goalID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE goal_review_works SET feedback_json='',control_revision=?,revision=revision+1,updated_at=?
			WHERE user_id=? AND goal_id=? AND work_id=?`, revision, now, user, goalID, w.WorkID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, advErr := s.advanceGoalReview(user, goalID, grMaxTickSteps, goalReviewNow()); advErr != nil && !errors.Is(advErr, ErrConflict) {
		return nil, advErr
	}
	return s.GoalReviewFeed(user, goalID, "")
}

func (s *Store) CancelGoalReview(user, goalID, requestID string, expected *int) (object, error) {
	goalID = strings.TrimSpace(goalID)
	requestID = strings.TrimSpace(requestID)
	if goalID == "" || requestID == "" || len(requestID) > 120 {
		return nil, ErrInvalid
	}
	var feed object
	err := s.write(func(tx *sql.Tx) error {
		f, err := readGoalReviewFollowTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if f.Deleted {
			return ErrNotFound
		}
		if expected != nil && *expected != f.ControlRevision {
			return ErrConflict
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w == nil {
			return ErrNotFound
		}
		switch w.Status {
		case "idle", "running", "saving", "failed", grWorkBlocked:
		default:
			return ErrInvalid
		}
		now := grNow()
		revision := f.ControlRevision + 1
		reviewJSON := w.ReviewJSON
		if reviewJSON != "" {
			if review, _ := parseObject([]byte(reviewJSON)); review != nil {
				review["status"] = "cancelled"
				review["cancelledAt"] = now
				review["result"] = nil
				review["resultId"] = ""
				review["updatedAt"] = now
				steps, _ := objects(review["steps"])
				for _, step := range steps {
					if str(step, "status") == "running" {
						step["status"] = "cancelled"
						step["error"] = "用户取消了这次复盘。"
					}
				}
				reviewJSON = encode(review)
			}
		}
		if _, err := tx.Exec(`UPDATE goal_review_follows SET control_revision=?,last_request_id=?,updated_at=? WHERE user_id=? AND goal_id=?`,
			revision, requestID, now, user, goalID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE goal_review_works SET status='cancelled',review_json=?,result_id='',control_revision=?,revision=revision+1,updated_at=?
			WHERE user_id=? AND goal_id=? AND work_id=?`, reviewJSON, revision, now, user, goalID, w.WorkID); err != nil {
			return err
		}
		feed, err = goalReviewFeedTx(tx, user, goalID, "")
		return err
	})
	return feed, err
}

func (s *Store) MarkGoalReviewSeen(user, goalID string, attentionRevision int) (object, error) {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" {
		return nil, ErrInvalid
	}
	var feed object
	err := s.write(func(tx *sql.Tx) error {
		f, err := readGoalReviewFollowTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if f.Deleted {
			return ErrNotFound
		}
		w, err := readGoalReviewWorkTx(tx, user, goalID)
		if err != nil {
			return err
		}
		if w == nil {
			return ErrNotFound
		}
		if attentionRevision < 0 || attentionRevision > w.AttentionRevision {
			return ErrConflict
		}
		target := w.AttentionRevision
		if attentionRevision > 0 {
			target = attentionRevision
		}
		if target > w.SeenAttentionRevision {
			if _, err := tx.Exec(`UPDATE goal_review_works SET seen_attention_revision=?,revision=revision+1,updated_at=?
				WHERE user_id=? AND goal_id=? AND work_id=?`, target, grNow(), user, goalID, w.WorkID); err != nil {
				return err
			}
		}
		feed, err = goalReviewFeedTx(tx, user, goalID, "")
		return err
	})
	return feed, err
}

// goalReviewOneStep runs at most one committed transition. Every transition
// re-reads the follow, the current input snapshot and the work row, so a pause,
// revocation, correction or feedback that landed first is respected.
func (s *Store) goalReviewOneStep(user, goalID string, now time.Time) (int, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var observed goalReviewSnapshot
		err := s.write(func(tx *sql.Tx) error {
			f, err := readGoalReviewFollowTx(tx, user, goalID)
			if err != nil {
				return err
			}
			if f.Deleted {
				observed.Follow = f
				return nil
			}
			observed, err = goalReviewSnapshotTx(tx, user, goalID)
			return err
		})
		if errors.Is(err, ErrNotFound) {
			return 0, true, nil
		}
		if err != nil {
			return 0, true, err
		}
		if err := goalReviewHook("after-snapshot"); err != nil {
			return 0, true, err
		}
		if observed.Follow.Deleted {
			return 0, true, nil
		}
		advanced := 0
		terminal := true
		err = s.write(func(tx *sql.Tx) error {
			advanced, terminal = 0, true
			f, err := readGoalReviewFollowTx(tx, user, goalID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if f.Deleted {
				return nil
			}
			snap, err := goalReviewSnapshotTx(tx, user, goalID)
			if err != nil {
				return err
			}
			if f.ControlRevision != observed.Follow.ControlRevision || snap.Digest != observed.Digest {
				return ErrConflict
			}
			if err := goalReviewHook("before-commit"); err != nil {
				return err
			}
			w, err := readGoalReviewWorkTx(tx, user, goalID)
			if err != nil {
				return err
			}
			if err := maintainGoalReviewCurrentnessTx(tx, user, goalID, w, snap); err != nil {
				return err
			}
			if !f.Enabled {
				// 暂停也继续清失效派生内容，但不生成新工作，也不因新资料或
				// 到期自动恢复。完成后的任务行删掉，避免 GET 仍报 completed。
				if w != nil && w.Digest != snap.Digest {
					if _, err := tx.Exec(`DELETE FROM goal_review_works WHERE user_id=? AND goal_id=?`, user, goalID); err != nil {
						return err
					}
					advanced = 1
				}
				return nil
			}
			if w != nil {
				fb := decodeGoalReviewFeedback(w.FeedbackJSON)
				if fb.Action == grFollowWaiting {
					return nil
				}
				if fb.Action == grFollowLater && !fb.Presented {
					due, err := time.Parse(time.RFC3339, fb.DueAt)
					if err != nil || now.Before(due) {
						return nil
					}
					if w.Digest == snap.Digest {
						if w.Status == "completed" || w.Status == grWorkBlocked || w.Status == "failed" {
							w.AttentionRevision++
							fb.Presented = true
							w.FeedbackJSON = encode(fb)
							w.ControlRevision = f.ControlRevision
							w.UpdatedAt = grNow()
							if err := updateGoalReviewWorkTx(tx, w, w.Revision, w.ExpectedControl); err != nil {
								return err
							}
							advanced = 1
							return nil
						}
					}
				}
			}
			if snap.Blocked != "" {
				if w != nil && w.Digest == snap.Digest && w.Status == grWorkBlocked {
					return nil
				}
				attention, seen, lastHash, resultRev := 0, 0, "", 0
				if w != nil {
					attention, seen, lastHash, resultRev = w.AttentionRevision, w.SeenAttentionRevision, w.LastBodyHash, w.ResultRevision
					if err := invalidateGoalReviewResultTx(tx, user, goalID, w.WorkID, grStaleInputReason); err != nil {
						return err
					}
				}
				nw := &goalReviewWork{
					UserID: user, GoalID: goalID, WorkID: grID(), Digest: snap.Digest,
					PartsJSON: encode(snap.Parts), Status: grWorkBlocked, BlockedReason: snap.Blocked,
					AttentionRevision: attention, SeenAttentionRevision: seen, LastBodyHash: lastHash,
					ResultRevision: resultRev, ControlRevision: f.ControlRevision, Revision: 0, CreatedAt: grNow(), UpdatedAt: grNow(),
				}
				if err := replaceGoalReviewWorkTx(tx, nw); err != nil {
					return err
				}
				advanced = 1
				return nil
			}
			if w != nil && w.Digest == snap.Digest {
				switch w.Status {
				case "completed", "cancelled", "stale", grWorkBlocked:
					return nil
				case "failed":
					review := object(nil)
					if w.ReviewJSON != "" {
						review, _ = parseObject([]byte(w.ReviewJSON))
					}
					if review == nil || mustInt(review["attempts"]) >= grMaxAttempts {
						return nil
					}
				}
				if w.ControlRevision != f.ControlRevision {
					w.ControlRevision = f.ControlRevision
				}
				terminal, err = s.goalReviewStepTx(tx, user, goalID, snap, w, now)
				if err != nil {
					return err
				}
				advanced = 1
				return nil
			}
			attention, seen, lastHash, resultRev := 0, 0, "", 0
			var prevParts goalReviewParts
			changeNote := ""
			var carryFeedback goalReviewFeedback
			if w != nil {
				attention, seen, lastHash, resultRev = w.AttentionRevision, w.SeenAttentionRevision, w.LastBodyHash, w.ResultRevision
				if w.PartsJSON != "" {
					_ = json.Unmarshal([]byte(w.PartsJSON), &prevParts)
				}
				changeNote = goalReviewChangeNote(prevParts, snap.Parts)
				carryFeedback = decodeGoalReviewFeedback(w.FeedbackJSON)
				if err := invalidateGoalReviewResultTx(tx, user, goalID, w.WorkID, grStaleInputReason); err != nil {
					return err
				}
			}
			state := object{"archiveId": "selfhost:" + user}
			review := grNewReview(state, snap.Goal, f.Request, grRefs(snap.Goal))
			if changeNote != "" {
				review["changeNote"] = changeNote
			}
			nw := &goalReviewWork{
				UserID: user, GoalID: goalID, WorkID: grID(), Digest: snap.Digest,
				PartsJSON: encode(snap.Parts), ReviewJSON: encode(review), Status: "idle",
				ChangeNote: changeNote, AttentionRevision: attention, SeenAttentionRevision: seen,
				LastBodyHash: lastHash, ResultRevision: resultRev, ControlRevision: f.ControlRevision, Revision: 0,
				CreatedAt: grNow(), UpdatedAt: grNow(),
			}
			switch carryFeedback.Action {
			case grFollowLater:
				if !carryFeedback.Presented {
					due, err := time.Parse(time.RFC3339, carryFeedback.DueAt)
					if err == nil && !now.Before(due) {
						carryFeedback.Presented = true
						nw.FeedbackJSON = encode(carryFeedback)
					}
				}
			case grFollowHandled, grFollowTodo:
				nw.FeedbackJSON = encode(carryFeedback)
			}
			if err := replaceGoalReviewWorkTx(tx, nw); err != nil {
				return err
			}
			terminal, err = s.goalReviewStepTx(tx, user, goalID, snap, nw, now)
			if err != nil {
				return err
			}
			advanced = 1
			return nil
		})
		if errors.Is(err, ErrConflict) {
			lastErr = err
			continue
		}
		if err != nil {
			return 0, true, err
		}
		if advanced > 0 && terminal {
			if err := goalReviewHook("save-readback"); err != nil {
				return advanced, terminal, err
			}
		}
		return advanced, terminal, nil
	}
	return 0, true, lastErr
}

func (s *Store) goalReviewStepTx(tx *sql.Tx, user, goalID string, snap goalReviewSnapshot, w *goalReviewWork, now time.Time) (bool, error) {
	review := object(nil)
	if w.ReviewJSON != "" {
		review, _ = parseObject([]byte(w.ReviewJSON))
	}
	state := object{"archiveId": "selfhost:" + user}
	if review == nil {
		review = grNewReview(state, snap.Goal, snap.Follow.Request, grRefs(snap.Goal))
	}
	review["engine"] = "server"
	stepID, done := grPrepare(state, snap.Goal, review)
	if done {
		w.ReviewJSON = encode(review)
		w.Status = str(review, "status")
		w.UpdatedAt = grNow()
		return true, s.persistGoalReviewWorkTx(tx, user, goalID, snap, w, review)
	}
	step := grStepByID(review, stepID)
	if step == nil {
		return false, ErrInvalid
	}
	outcome, reason := grExecute(snap.D, state, snap.Goal, review, stepID)
	terminal := false
	switch outcome {
	case "done":
		step["status"] = "done"
		step["finishedAt"] = grNow()
		review["status"] = "running"
	case "complete":
		grCompleteSave(state, snap.Goal, review)
		terminal = true
	case "saving":
		terminal = true
	case "stale":
		step["status"] = "cancelled"
		step["finishedAt"] = grNow()
		step["error"] = reason
		terminal = true
	case "failed":
		step["status"] = "failed"
		step["finishedAt"] = grNow()
		step["error"] = reason
		review["status"] = "failed"
		review["error"] = reason
		terminal = true
	case "cancelled":
		step["status"] = "cancelled"
		step["finishedAt"] = grNow()
		terminal = true
	case "retry":
		step["status"] = "pending"
		step["resumedAt"] = grNow()
		review["status"] = "running"
	}
	review["updatedAt"] = grNow()
	if outcome == "saving" {
		if grCompleteSave(state, snap.Goal, review) {
			outcome = "complete"
			terminal = true
		}
	}
	if str(review, "status") == "completed" {
		if err := grValidateReview(snap.D, snap.Goal, review); err != nil {
			review["status"] = "failed"
			review["error"] = "保存后的复盘未通过校验。"
			review["result"] = nil
			review["resultId"] = ""
			outcome = "failed"
			terminal = true
		}
	}
	w.ReviewJSON = encode(review)
	w.Status = str(review, "status")
	w.ChangeNote = str(review, "changeNote")
	w.ResultID = str(review, "resultId")
	w.UpdatedAt = grNow()
	if err := s.persistGoalReviewWorkTx(tx, user, goalID, snap, w, review); err != nil {
		return false, err
	}
	if outcome == "saving" || outcome == "complete" || outcome == "failed" || outcome == "stale" || outcome == "cancelled" {
		terminal = true
	}
	return terminal, nil
}

func (s *Store) persistGoalReviewWorkTx(tx *sql.Tx, user, goalID string, snap goalReviewSnapshot, w *goalReviewWork, review object) error {
	if str(review, "status") == "completed" {
		result, _ := review["result"].(object)
		if result == nil || str(result, "id") == "" || str(result, "id") != str(review, "resultId") {
			return ErrInvalid
		}
		resultID := str(result, "id")
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM goal_review_results WHERE user_id=? AND goal_id=? AND result_id=?`,
			user, goalID, resultID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			body := str(result, "body")
			if body == "" {
				return ErrInvalid
			}
			bodyHash := goalReviewTextHash(body)
			if bodyHash != w.LastBodyHash {
				w.AttentionRevision++
				fb := decodeGoalReviewFeedback(w.FeedbackJSON)
				if fb.Action == grFollowHandled || fb.Action == grFollowTodo {
					w.FeedbackJSON = ""
				}
			}
			w.LastBodyHash = bodyHash
			w.ResultRevision++
			if _, err := tx.Exec(`INSERT INTO goal_review_results
				(user_id,goal_id,work_id,result_id,digest,body,result_json,invalidated,invalidated_reason,created_at)
				VALUES(?,?,?,?,?,?,?,0,'',?)`,
				user, goalID, w.WorkID, resultID, w.Digest, body, encode(result), grNow()); err != nil {
				return err
			}
			w.ResultID = resultID
		}
	} else {
		w.ResultID = ""
	}
	w.ControlRevision = snap.Follow.ControlRevision
	return updateGoalReviewWorkTx(tx, w, w.Revision, w.ExpectedControl)
}

func (s *Store) advanceGoalReview(user, goalID string, steps int, now time.Time) (int, error) {
	if steps <= 0 {
		steps = grMaxTickSteps
	}
	total := 0
	for i := 0; i < steps; i++ {
		n, done, err := s.goalReviewOneStep(user, goalID, now)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				return total, nil
			}
			return total, err
		}
		total += n
		if done || n == 0 {
			break
		}
	}
	return total, nil
}

func (s *Store) AdvanceGoalReview(user, goalID string, steps int, now time.Time) (int, error) {
	if goalID == "" {
		return 0, ErrInvalid
	}
	return s.advanceGoalReview(user, goalID, steps, now)
}

func (s *Store) RunGoalReviewTick(user string, now time.Time) (int, error) {
	if now.IsZero() {
		now = goalReviewNow()
	}
	rows, err := s.db.Query(`SELECT goal_id FROM goal_review_follows WHERE user_id=? AND deleted=0 ORDER BY goal_id`, user)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	made := 0
	var firstErr error
	for _, id := range ids {
		n, err := s.advanceGoalReview(user, id, grMaxTickSteps, now)
		made += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return made, firstErr
}

func (s *Store) RunGoalReviewTickAll(now time.Time) (int, error) {
	if now.IsZero() {
		now = goalReviewNow()
	}
	rows, err := s.db.Query(`SELECT DISTINCT user_id FROM goal_review_follows WHERE deleted=0 ORDER BY user_id`)
	if err != nil {
		return 0, err
	}
	var users []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	made := 0
	var firstErr error
	for _, user := range users {
		n, err := s.RunGoalReviewTick(user, now)
		made += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return made, firstErr
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
