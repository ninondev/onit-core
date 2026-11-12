package main

// 自部署服务端的主动准备：把本机已有的规则式材料简报接到服务端，
// 但只读地投影服务端自己的来源、事实和目标，绝不回写 Android 的 slot=1
// 档案。每份准备按 (user_id, client_key) 归属；策略、输出和运行状态分开
// 持久化，输出带 revision 与快照哈希，提交时做 CAS 复核。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const selfHostPreparationSchema = `
CREATE TABLE IF NOT EXISTS preparation_policies (
	user_id TEXT NOT NULL REFERENCES users(id),
	client_key TEXT NOT NULL,
	goal_id TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 0,
	version INTEGER NOT NULL DEFAULT 1,
	timezone TEXT NOT NULL,
	request TEXT NOT NULL DEFAULT '',
	selected_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY(user_id, client_key)
);
CREATE TABLE IF NOT EXISTS preparation_outputs (
	user_id TEXT NOT NULL REFERENCES users(id),
	client_key TEXT NOT NULL,
	goal_id TEXT NOT NULL,
	revision INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT '',
	xml TEXT NOT NULL DEFAULT '',
	basis_json TEXT NOT NULL DEFAULT '[]',
	fingerprint TEXT NOT NULL DEFAULT '',
	snapshot_hash TEXT NOT NULL DEFAULT '',
	policy_version INTEGER NOT NULL DEFAULT 0,
	date TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT '',
	deleted INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(user_id, client_key)
);
CREATE TABLE IF NOT EXISTS preparation_runs (
	user_id TEXT NOT NULL,
	client_key TEXT NOT NULL,
	last_attempt_at TEXT NOT NULL DEFAULT '',
	last_success_at TEXT NOT NULL DEFAULT '',
	last_result TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	last_revision INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(user_id, client_key)
);`

const preparationOutputColumns = "goal_id,revision,status,xml,basis_json,fingerprint,snapshot_hash,policy_version,date,updated_at,deleted"

type preparationRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type preparationPolicy struct {
	UserID    string           `json:"-"`
	ClientKey string           `json:"clientKey"`
	GoalID    string           `json:"goalId"`
	Enabled   bool             `json:"enabled"`
	Version   int              `json:"version"`
	Timezone  string           `json:"timezone"`
	Request   string           `json:"request"`
	Selected  []preparationRef `json:"selected"`
	CreatedAt string           `json:"createdAt"`
	UpdatedAt string           `json:"updatedAt"`
}

type preparationOutput struct {
	UserID        string
	ClientKey     string
	GoalID        string
	Revision      int
	Status        string
	XML           string
	Basis         []proactiveBasis
	Fingerprint   string
	SnapshotHash  string
	PolicyVersion int
	Date          string
	UpdatedAt     string
	Deleted       bool
}

// preparationFactSpan 记录选中事实在它真实来源里的引用位置内容。
// 它只参与依赖复核和来源标签，不会被当作生成材料。
type preparationFactSpan struct {
	FactID      string
	SourceID    string
	SourceTitle string
	Start       int
	End         int
	Content     string
	Supported   bool
}

type preparationSnapshot struct {
	Policy      preparationPolicy
	Goal        Goal
	Sources     []Source
	Facts       []Memory
	FactSources []Source
	FactSpans   []preparationFactSpan
	FactIssues  []string
	Missing     []preparationRef
	Output      *preparationOutput
	Hash        string
}

var (
	preparationHookMu sync.Mutex
	preparationHookFn func(string) error
)

// preparationHook 是测试用的故障/竞态注入点：after-snapshot 在读快照后、
// before-commit 在提交前、after-commit 在提交后。生产路径为空。
func preparationHook(stage string) error {
	preparationHookMu.Lock()
	fn := preparationHookFn
	preparationHookMu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(stage)
}

func setPreparationHook(fn func(string) error) {
	preparationHookMu.Lock()
	preparationHookFn = fn
	preparationHookMu.Unlock()
}

var preparationClientKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)

func validPreparationClientKey(key string) bool {
	return preparationClientKeyPattern.MatchString(key)
}

func prepBoolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func preparationAccount(user string) string {
	sum := sha256.Sum256([]byte("onit-selfhost-account\x00" + user))
	return hex.EncodeToString(sum[:8])
}

func encodePreparationRefs(refs []preparationRef) string {
	raw, _ := json.Marshal(refs)
	return string(raw)
}

func preparationRefsAny(refs []preparationRef) []any {
	out := make([]any, len(refs))
	for i, ref := range refs {
		out[i] = object{"kind": ref.Kind, "id": ref.ID}
	}
	return out
}

func samePreparationRefs(a, b []preparationRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func decodePreparationBasis(raw string) []proactiveBasis {
	if raw == "" || raw == "[]" {
		return nil
	}
	var rows []object
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil
	}
	out := make([]proactiveBasis, 0, len(rows))
	for _, row := range rows {
		out = append(out, proactiveBasis{Kind: str(row, "kind"), ID: str(row, "id"), Version: int(number(row["version"]))})
	}
	return out
}

func basisFromAny(v any) []proactiveBasis {
	rows := objectsOrEmpty(v)
	out := make([]proactiveBasis, 0, len(rows))
	for _, row := range rows {
		out = append(out, proactiveBasis{Kind: str(row, "kind"), ID: str(row, "id"), Version: int(number(row["version"]))})
	}
	return out
}

func readPreparationPolicyTx(tx *sql.Tx, user, clientKey string) (preparationPolicy, error) {
	var p preparationPolicy
	var selected string
	var enabled int
	err := tx.QueryRow(`SELECT goal_id,enabled,version,timezone,request,selected_json,created_at,updated_at
		FROM preparation_policies WHERE user_id=? AND client_key=?`, user, clientKey).
		Scan(&p.GoalID, &enabled, &p.Version, &p.Timezone, &p.Request, &selected, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, missing(err)
	}
	p.UserID = user
	p.ClientKey = clientKey
	p.Enabled = enabled != 0
	if err := json.Unmarshal([]byte(selected), &p.Selected); err != nil {
		return p, err
	}
	return p, nil
}

func scanPreparationOutput(row rowScanner) (preparationOutput, error) {
	var out preparationOutput
	var basis string
	var deleted int
	if err := row.Scan(&out.GoalID, &out.Revision, &out.Status, &out.XML, &basis, &out.Fingerprint, &out.SnapshotHash, &out.PolicyVersion, &out.Date, &out.UpdatedAt, &deleted); err != nil {
		return out, missing(err)
	}
	out.Deleted = deleted != 0
	out.Basis = decodePreparationBasis(basis)
	return out, nil
}

func readPreparationOutputTx(tx *sql.Tx, user, clientKey string, out *preparationOutput) error {
	row, err := scanPreparationOutput(tx.QueryRow("SELECT "+preparationOutputColumns+" FROM preparation_outputs WHERE user_id=? AND client_key=?", user, clientKey))
	if err != nil {
		return err
	}
	row.UserID = user
	row.ClientKey = clientKey
	*out = row
	return nil
}

func writePreparationOutputTx(tx *sql.Tx, out preparationOutput) error {
	_, err := tx.Exec(`INSERT INTO preparation_outputs(user_id,client_key,goal_id,revision,status,xml,basis_json,fingerprint,snapshot_hash,policy_version,date,updated_at,deleted)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id,client_key) DO UPDATE SET
			goal_id=excluded.goal_id,revision=excluded.revision,status=excluded.status,xml=excluded.xml,
			basis_json=excluded.basis_json,fingerprint=excluded.fingerprint,snapshot_hash=excluded.snapshot_hash,
			policy_version=excluded.policy_version,date=excluded.date,updated_at=excluded.updated_at,deleted=excluded.deleted`,
		out.UserID, out.ClientKey, out.GoalID, out.Revision, out.Status, out.XML, encode(toAnyBasis(out.Basis)),
		out.Fingerprint, out.SnapshotHash, out.PolicyVersion, out.Date, out.UpdatedAt, prepBoolInt(out.Deleted))
	return err
}

func recordPreparationRunTx(tx *sql.Tx, user, clientKey, result string, revision int, failure, at string) error {
	success := ""
	if result == "ok" || result == "noop" || result == "stale" {
		success = at
	}
	_, err := tx.Exec(`INSERT INTO preparation_runs(user_id,client_key,last_attempt_at,last_success_at,last_result,last_error,last_revision)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(user_id,client_key) DO UPDATE SET
			last_attempt_at=excluded.last_attempt_at,
			last_success_at=CASE WHEN excluded.last_success_at<>'' THEN excluded.last_success_at ELSE preparation_runs.last_success_at END,
			last_result=excluded.last_result,
			last_error=excluded.last_error,
			last_revision=CASE WHEN excluded.last_result='failed' THEN preparation_runs.last_revision ELSE excluded.last_revision END`,
		user, clientKey, at, success, result, failure, revision)
	return err
}

func (s *Store) recordPreparationFailure(user, clientKey string, now time.Time, cause error) {
	if cause == nil {
		cause = errors.New("preparation failed")
	}
	_ = s.write(func(tx *sql.Tx) error {
		return recordPreparationRunTx(tx, user, clientKey, "failed", 0, cause.Error(), now.UTC().Format(time.RFC3339))
	})
}

func (s *Store) recordPreparationRun(user, clientKey, result string, snap preparationSnapshot, now time.Time) error {
	revision := 0
	if snap.Output != nil {
		revision = snap.Output.Revision
	}
	return s.write(func(tx *sql.Tx) error {
		return recordPreparationRunTx(tx, user, clientKey, result, revision, "", now.UTC().Format(time.RFC3339))
	})
}

// SavePreparationPolicy 只接受认证 owner 自己的目标和材料；外部 ref 与不存在
// 的 ref 返回同一个 ErrNotFound，不泄露其他用户的内容。只有内容真正变化
// 才增加 version；暂停或范围变化时，同一事务内把旧输出降为 stale。
func (s *Store) SavePreparationPolicy(user, clientKey, goalID string, enabled bool, timezone, request string, selected []preparationRef) (preparationPolicy, error) {
	clientKey = strings.TrimSpace(clientKey)
	goalID = strings.TrimSpace(goalID)
	timezone = strings.TrimSpace(timezone)
	request = strings.TrimSpace(request)
	if !validPreparationClientKey(clientKey) || goalID == "" || len(goalID) > 120 {
		return preparationPolicy{}, ErrInvalid
	}
	if timezone == "" || len(timezone) > 80 || timezone == "Local" {
		return preparationPolicy{}, ErrInvalid
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return preparationPolicy{}, ErrInvalid
	}
	if utf8.RuneCountInString(request) > 4000 {
		return preparationPolicy{}, ErrInvalid
	}
	if len(selected) > 40 {
		return preparationPolicy{}, ErrInvalid
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
			return preparationPolicy{}, ErrInvalid
		}
		key := kind + ":" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, preparationRef{Kind: kind, ID: id})
	}
	if enabled && (request == "" || len(clean) == 0) {
		return preparationPolicy{}, ErrInvalid
	}
	var result preparationPolicy
	err := s.write(func(tx *sql.Tx) error {
		old, oldErr := readPreparationPolicyTx(tx, user, clientKey)
		existing := oldErr == nil
		if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
			return oldErr
		}
		var exists int
		if err := tx.QueryRow("SELECT COUNT(1) FROM goals WHERE user_id=? AND id=?", user, goalID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		// 暂停一份已有准备时，选中的材料可能已经被撤回；暂停本身仍要成功，
		// 只是不能再生成。重新启用时会重新核对 refs，缺材料就要求重选。
		if enabled || !existing {
			for _, ref := range clean {
				table := "memories"
				if ref.Kind == "source" {
					table = "sources"
				}
				if err := tx.QueryRow("SELECT COUNT(1) FROM "+table+" WHERE user_id=? AND id=?", user, ref.ID).Scan(&exists); err != nil {
					return err
				}
				if exists == 0 {
					return ErrNotFound
				}
			}
		}
		now := time.Now().UTC().Format(time.RFC3339)
		version := 1
		changed := true
		createdAt := now
		var err error
		if existing {
			version = old.Version
			createdAt = old.CreatedAt
			changed = old.GoalID != goalID || old.Enabled != enabled || old.Timezone != timezone || old.Request != request || !samePreparationRefs(old.Selected, clean)
			if changed {
				version++
			}
			_, err = tx.Exec(`UPDATE preparation_policies SET goal_id=?,enabled=?,version=?,timezone=?,request=?,selected_json=?,updated_at=? WHERE user_id=? AND client_key=?`,
				goalID, prepBoolInt(enabled), version, timezone, request, encodePreparationRefs(clean), now, user, clientKey)
		} else {
			_, err = tx.Exec(`INSERT INTO preparation_policies(user_id,client_key,goal_id,enabled,version,timezone,request,selected_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				user, clientKey, goalID, prepBoolInt(enabled), version, timezone, request, encodePreparationRefs(clean), now, now)
		}
		if err != nil {
			return err
		}
		result = preparationPolicy{UserID: user, ClientKey: clientKey, GoalID: goalID, Enabled: enabled, Version: version, Timezone: timezone, Request: request, Selected: clean, CreatedAt: createdAt, UpdatedAt: now}
		if changed || !enabled {
			var out preparationOutput
			outErr := readPreparationOutputTx(tx, user, clientKey, &out)
			if outErr == nil && !out.Deleted && out.Status != "stale" {
				out.Revision++
				out.Status = "stale"
				if !enabled {
					out.XML = preparationNoticeXML(out.GoalID, "stale", "这份服务器准备已暂停", "暂停后旧内容不再显示；继续时会按当前材料重新准备。")
				} else {
					out.XML = preparationNoticeXML(out.GoalID, "stale", "这份服务器准备的材料范围已变化", "旧内容不再显示；下一次准备会按新的选择重做。")
				}
				out.Basis = nil
				out.Fingerprint = ""
				out.SnapshotHash = ""
				out.PolicyVersion = version
				out.UpdatedAt = now
				if err := writePreparationOutputTx(tx, out); err != nil {
					return err
				}
			} else if outErr != nil && !errors.Is(outErr, ErrNotFound) {
				return outErr
			}
		}
		return nil
	})
	if err != nil {
		return preparationPolicy{}, err
	}
	return result, nil
}

// ClearPreparation 暂停并墓碑化输出。旧 XML 立即换成不含原文的失效说明，
// 客户端按 deleted 移除；重新启用会得到新的 revision，不会复活旧内容。
func (s *Store) ClearPreparation(user, clientKey string) error {
	clientKey = strings.TrimSpace(clientKey)
	if !validPreparationClientKey(clientKey) {
		return ErrInvalid
	}
	return s.write(func(tx *sql.Tx) error {
		policy, err := readPreparationPolicyTx(tx, user, clientKey)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		version := policy.Version + 1
		if _, err := tx.Exec(`UPDATE preparation_policies SET enabled=0,version=?,updated_at=? WHERE user_id=? AND client_key=?`, version, now, user, clientKey); err != nil {
			return err
		}
		var out preparationOutput
		if outErr := readPreparationOutputTx(tx, user, clientKey, &out); outErr == nil {
			out.Revision++
			out.Status = "stale"
			out.XML = preparationNoticeXML(out.GoalID, "stale", "这份服务器准备已清除", "你已清除这份准备，旧内容不再显示。")
			out.Basis = nil
			out.Fingerprint = ""
			out.SnapshotHash = ""
			out.PolicyVersion = version
			out.UpdatedAt = now
			out.Deleted = true
			return writePreparationOutputTx(tx, out)
		} else if !errors.Is(outErr, ErrNotFound) {
			return outErr
		}
		return nil
	})
}

func preparationSnapshotHash(snap preparationSnapshot) string {
	type refShape struct {
		Kind string
		ID   string
	}
	type sourceShape struct {
		ID, Title, Kind, Origin, Text string
	}
	type factShape struct {
		ID, SourceID, Text, Quote string
		Version                   int
		Confirmed                 bool
	}
	type factSpanShape struct {
		FactID, SourceID, SourceTitle, Content string
		Start, End                             int
		Supported                              bool
	}
	type goalShape struct {
		ID, Title, Step, Unit string
		Methods               []string
	}
	refs := make([]refShape, len(snap.Policy.Selected))
	for i, ref := range snap.Policy.Selected {
		refs[i] = refShape{ref.Kind, ref.ID}
	}
	sources := make([]sourceShape, len(snap.Sources))
	for i, src := range snap.Sources {
		sources[i] = sourceShape{src.ID, src.Title, src.Kind, src.Origin, src.Text}
	}
	facts := make([]factShape, len(snap.Facts))
	for i, m := range snap.Facts {
		facts[i] = factShape{m.ID, m.SourceID, m.Text, m.Quote, m.Version, m.Confirmed}
	}
	factSpans := make([]factSpanShape, len(snap.FactSpans))
	for i, span := range snap.FactSpans {
		factSpans[i] = factSpanShape{span.FactID, span.SourceID, span.SourceTitle, span.Content, span.Start, span.End, span.Supported}
	}
	methods := make([]string, len(snap.Goal.ApprovedMethods))
	for i, m := range snap.Goal.ApprovedMethods {
		methods[i] = m.Origin + ":" + m.Text
	}
	missing := make([]refShape, len(snap.Missing))
	for i, ref := range snap.Missing {
		missing[i] = refShape{ref.Kind, ref.ID}
	}
	payload := struct {
		PolicyVersion int
		Enabled       bool
		Timezone      string
		Request       string
		Selected      []refShape
		Goal          goalShape
		Sources       []sourceShape
		Facts         []factShape
		FactSpans     []factSpanShape
		FactIssues    []string
		Missing       []refShape
	}{
		snap.Policy.Version, snap.Policy.Enabled, snap.Policy.Timezone, snap.Policy.Request, refs,
		goalShape{snap.Goal.ID, snap.Goal.Title, snap.Goal.Step, snap.Goal.Unit, methods},
		sources, facts, factSpans, snap.FactIssues, missing,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// preparationFactSupport 判断事实的引文是否仍由来源正文支撑。
// 先核对记录的真实引用位置；位置对不上时，只要同一引文仍在来源里，
// 仍算有依据（来源可能只是插入了别的内容）。引文彻底消失才算失效。
func preparationFactSupport(body string, start, end int, quote string) (string, bool) {
	if quote == "" {
		return "", true
	}
	if start >= 0 && end >= start && end <= len(body) {
		segment := body[start:end]
		if segment == quote {
			return segment, true
		}
	}
	if strings.Contains(body, quote) {
		return quote, true
	}
	return "", false
}

// selectedMaterialSet 是显式选中的事实/来源及其真实引用核对结果。
// 材料简报和持续复盘共用这一套规则，避免两条路径对同一份材料
// 给出不同的有效性判断。
type selectedMaterialSet struct {
	Facts       []Memory
	Sources     []Source
	FactSources []Source
	FactSpans   []preparationFactSpan
	FactIssues  []string
	Missing     []preparationRef
}

// resolveSelectedMaterialsTx 只读取被显式选中的事实/来源。事实的正文可能
// 来自它自己的来源；来源正文变了，即使事实行没变，旧引文也不再成立。
// 未选中的整份来源不会因此进入生成材料。
func resolveSelectedMaterialsTx(tx *sql.Tx, user string, selected []preparationRef) (selectedMaterialSet, error) {
	var set selectedMaterialSet
	for _, ref := range selected {
		switch ref.Kind {
		case "fact":
			m, _, err := readMemory(tx.QueryRow("SELECT "+memoryColumns+" FROM memories WHERE user_id=? AND id=?", user, ref.ID))
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					set.Missing = append(set.Missing, ref)
					continue
				}
				return set, err
			}
			if m.SourceID != "" {
				var start, end int
				if err := tx.QueryRow(`SELECT start_byte,end_byte FROM memories WHERE user_id=? AND id=?`, user, ref.ID).Scan(&start, &end); err != nil {
					if errors.Is(missing(err), ErrNotFound) {
						set.Missing = append(set.Missing, ref)
						continue
					}
					return set, err
				}
				src, err := readSource(tx.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, m.SourceID))
				if err != nil {
					if errors.Is(err, ErrNotFound) {
						set.FactIssues = append(set.FactIssues, "fact-source-missing")
						set.Missing = append(set.Missing, ref)
						continue
					}
					return set, err
				}
				content, supported := preparationFactSupport(src.Text, start, end, m.Quote)
				set.FactSpans = append(set.FactSpans, preparationFactSpan{
					FactID: m.ID, SourceID: m.SourceID, SourceTitle: src.Title,
					Start: start, End: end, Content: content, Supported: supported,
				})
				if !supported {
					set.FactIssues = append(set.FactIssues, "fact-source-changed")
					set.Missing = append(set.Missing, ref)
					continue
				}
				set.FactSources = append(set.FactSources, src)
			}
			set.Facts = append(set.Facts, m)
		case "source":
			src, err := readSource(tx.QueryRow("SELECT "+sourceColumns+" FROM sources WHERE user_id=? AND id=?", user, ref.ID))
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					set.Missing = append(set.Missing, ref)
					continue
				}
				return set, err
			}
			set.Sources = append(set.Sources, src)
		default:
			return set, ErrInvalid
		}
	}
	return set, nil
}

func preparationSnapshotTx(tx *sql.Tx, user, clientKey string) (preparationSnapshot, error) {
	var snap preparationSnapshot
	policy, err := readPreparationPolicyTx(tx, user, clientKey)
	if err != nil {
		return snap, err
	}
	snap.Policy = policy
	var g Goal
	err = tx.QueryRow(`SELECT id,title,step,unit,created_at FROM goals WHERE user_id=? AND id=?`, user, policy.GoalID).
		Scan(&g.ID, &g.Title, &g.Step, &g.Unit, &g.At)
	if err != nil {
		return snap, missing(err)
	}
	var methods string
	_ = tx.QueryRow(`SELECT methods FROM goal_choices WHERE goal_id=?`, policy.GoalID).Scan(&methods)
	if methods != "" {
		_ = json.Unmarshal([]byte(methods), &g.ApprovedMethods)
	}
	snap.Goal = g
	set, err := resolveSelectedMaterialsTx(tx, user, policy.Selected)
	if err != nil {
		return snap, err
	}
	snap.Facts = set.Facts
	snap.Sources = set.Sources
	snap.FactSources = set.FactSources
	snap.FactSpans = set.FactSpans
	snap.FactIssues = set.FactIssues
	snap.Missing = set.Missing
	var out preparationOutput
	if err := readPreparationOutputTx(tx, user, clientKey, &out); err == nil {
		snap.Output = &out
	} else if !errors.Is(err, ErrNotFound) {
		return snap, err
	}
	snap.Hash = preparationSnapshotHash(snap)
	return snap, nil
}

func (s *Store) preparationSnapshot(user, clientKey string) (preparationSnapshot, error) {
	var snap preparationSnapshot
	err := s.write(func(tx *sql.Tx) error {
		var err error
		snap, err = preparationSnapshotTx(tx, user, clientKey)
		return err
	})
	return snap, err
}

func (s *Store) preparationKeys(user string) ([]string, error) {
	rows, err := s.db.Query("SELECT client_key FROM preparation_policies WHERE user_id=? ORDER BY client_key", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func preparationGoal(snap preparationSnapshot) object {
	refs := make([]any, len(snap.Policy.Selected))
	for i, ref := range snap.Policy.Selected {
		refs[i] = object{"kind": ref.Kind, "id": ref.ID}
	}
	methods := make([]any, 0, len(snap.Goal.ApprovedMethods))
	for _, m := range snap.Goal.ApprovedMethods {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		methods = append(methods, object{"text": m.Text})
	}
	return object{
		"id": snap.Goal.ID, "title": snap.Goal.Title, "step": snap.Goal.Step, "unit": snap.Goal.Unit,
		"brief":           object{"request": snap.Policy.Request, "materialRefs": refs, "textMode": "system"},
		"approvedMethods": methods,
	}
}

func preparationSources(snap preparationSnapshot) []object {
	out := make([]object, 0, len(snap.Sources))
	for _, src := range snap.Sources {
		out = append(out, object{"id": src.ID, "title": src.Title, "kind": src.Kind, "origin": src.Origin, "text": src.Text})
	}
	return out
}

func preparationFacts(snap preparationSnapshot) []object {
	out := make([]object, 0, len(snap.Facts))
	for _, m := range snap.Facts {
		out = append(out, object{"id": m.ID, "sourceId": m.SourceID, "text": m.Text, "quote": m.Quote, "version": m.Version, "confirmed": m.Confirmed})
	}
	return out
}

// preparationStateSources 只给状态里的来源标题补上事实的真实来源。
// 未显式选择的来源不进入生成材料，正文留空，避免被当成整份材料。
func preparationStateSources(snap preparationSnapshot) []object {
	out := preparationSources(snap)
	seen := make(map[string]bool, len(snap.Sources))
	for _, src := range snap.Sources {
		seen[src.ID] = true
	}
	for _, src := range snap.FactSources {
		if seen[src.ID] {
			continue
		}
		seen[src.ID] = true
		out = append(out, object{"id": src.ID, "title": src.Title, "kind": src.Kind, "origin": src.Origin, "text": ""})
	}
	return out
}

func preparationCardID(goalID string) string {
	return "widget:prepared-brief-" + goalID
}

func preparationCard(out preparationOutput) object {
	return object{
		"id": preparationCardID(out.GoalID), "date": out.Date, "type": "prepared",
		"xml": out.XML, "goalId": out.GoalID,
		"proactive": object{
			"kind": "material-brief", "key": "brief:" + out.GoalID, "status": out.Status,
			"fingerprint": out.Fingerprint, "basis": toAnyBasis(out.Basis),
		},
	}
}

func preparationCardTitle(xml string) string {
	const open, close = "<title>", "</title>"
	start := strings.Index(xml, open)
	end := strings.Index(xml, close)
	if start >= 0 && end > start+len(open) {
		title := strings.TrimSpace(xml[start+len(open) : end])
		if title != "" {
			return title
		}
	}
	return "材料简报"
}

// preparationOutputIsCurrent is the live currentness check: stored snapshot hash
// still equals the current policy and selected-material snapshot, and the
// card is ready with a body. Review input uses this without writing new briefs.
func preparationOutputIsCurrent(out preparationOutput, snap preparationSnapshot) bool {
	return !out.Deleted && out.Status == "ready" && strings.TrimSpace(out.XML) != "" &&
		out.SnapshotHash != "" && out.SnapshotHash == snap.Hash
}

// currentPreparationCardsTx projects the owner's current, actually readable
// Prepared briefs for this goal into the shared review input. Other
// owners, other goals, empty/forged rows, withdrawn or corrected basis and
// Review's own results stay out. Currentness is the snapshot hash, not mere
// existence of the original refs.
func currentPreparationCardsTx(tx *sql.Tx, user, goalID string) ([]any, map[string]string, error) {
	cards := []any{}
	hashes := map[string]string{}
	rows, err := tx.Query(`SELECT client_key FROM preparation_outputs WHERE user_id=? AND goal_id=? AND deleted=0 ORDER BY client_key`, user, goalID)
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	for _, key := range keys {
		var out preparationOutput
		if err := readPreparationOutputTx(tx, user, key, &out); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, nil, err
		}
		if out.GoalID != goalID {
			continue
		}
		snap, err := preparationSnapshotTx(tx, user, key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, nil, err
		}
		if !preparationOutputIsCurrent(out, snap) {
			continue
		}
		card := preparationCard(out)
		title := preparationCardTitle(out.XML)
		card["title"] = title
		if proactive, ok := card["proactive"].(object); ok && proactive != nil {
			proactive["title"] = title
		}
		id := str(card, "id")
		if id == "" {
			continue
		}
		cards = append(cards, card)
		hashes[id] = out.SnapshotHash
	}
	return cards, hashes, nil
}

func preparationState(snap preparationSnapshot, now time.Time) object {
	state := object{
		"goals":       []any{preparationGoal(snap)},
		"sources":     toAnyObjects(preparationStateSources(snap)),
		"facts":       toAnyObjects(preparationFacts(snap)),
		"discoveries": []any{},
		"artifacts":   []any{},
		"proactive":   object{"dismissed": []any{}},
	}
	if snap.Output != nil && !snap.Output.Deleted {
		state["artifacts"] = []any{preparationCard(*snap.Output)}
	}
	return state
}

func preparationNoticeXML(goalID, status, title, why string) string {
	badge := "已失效"
	switch status {
	case "missing":
		badge = "缺少材料"
	case "closed":
		badge = "已收尾"
	}
	return `<prepared id="` + xmlEscape(preparationCardID(goalID)) + `" kind="material-brief" status="` + xmlEscape(status) + `">` +
		`<badge>` + xmlEscape(badge) + `</badge><title>` + xmlEscape(title) + `</title>` +
		`<why>` + xmlEscape(why) + `</why>` +
		`<question>这份准备暂时不能用。</question></prepared>`
}

func (s *Store) preparationNext(snap preparationSnapshot, now time.Time) (preparationOutput, bool, int, error) {
	loc, err := time.LoadLocation(snap.Policy.Timezone)
	if err != nil {
		return preparationOutput{}, false, 0, ErrInvalid
	}
	localNow := now.In(loc)
	base := preparationOutput{UserID: snap.Policy.UserID, ClientKey: snap.Policy.ClientKey, GoalID: snap.Policy.GoalID}
	if snap.Output != nil {
		base = *snap.Output
	}
	if snap.Output != nil && !snap.Output.Deleted && snap.Output.Status != "stale" && snap.Output.SnapshotHash != snap.Hash {
		next := base
		next.Revision++
		next.Status = "stale"
		next.XML = preparationNoticeXML(next.GoalID, "stale", "这份服务器准备已失效", "依据已经撤回或发生变化，旧内容不再显示。")
		next.Basis = nil
		next.SnapshotHash = snap.Hash
		next.PolicyVersion = snap.Policy.Version
		next.UpdatedAt = now.UTC().Format(time.RFC3339)
		return next, true, 0, nil
	}
	if !snap.Policy.Enabled {
		return preparationOutput{}, false, 0, nil
	}
	state := preparationState(snap, localNow)
	existing := map[string]object{}
	if snap.Output != nil && !snap.Output.Deleted {
		existing[preparationCardID(snap.Policy.GoalID)] = preparationCard(*snap.Output)
	}
	card, ok := buildMaterialBrief(state, preparationGoal(snap), preparationSources(snap), preparationFacts(snap), nil, localNow, existing, map[string]bool{})
	if ok {
		proactive := objectOrEmpty(card["proactive"])
		fp := str(proactive, "fingerprint")
		xml := str(card, "xml")
		changed := snap.Output == nil || snap.Output.Deleted || snap.Output.Status != "ready" ||
			snap.Output.Fingerprint != fp || snap.Output.XML != xml ||
			snap.Output.SnapshotHash != snap.Hash || snap.Output.PolicyVersion != snap.Policy.Version
		if !changed {
			return preparationOutput{}, false, 0, nil
		}
		next := base
		// 墓碑也保留 revision：同一 owner/key 跨 delete/re-enable 必须单调，
		// 否则客户端会把合法的新结果当成旧版本丢掉。
		if snap.Output == nil {
			next.Revision = 1
		} else {
			next.Revision = snap.Output.Revision + 1
		}
		next.Status = "ready"
		next.XML = xml
		next.Basis = basisFromAny(proactive["basis"])
		next.Fingerprint = fp
		next.SnapshotHash = snap.Hash
		next.PolicyVersion = snap.Policy.Version
		next.Date = str(card, "date")
		next.UpdatedAt = now.UTC().Format(time.RFC3339)
		next.Deleted = false
		made := 0
		if snap.Output == nil || snap.Output.Deleted || snap.Output.Fingerprint != fp {
			made = 1
		}
		return next, true, made, nil
	}
	// buildMaterialBrief also returns false when the existing card is already
	// up to date. That is not a missing-material state: keep the ready output
	// and report no change, otherwise every retry would replace a good card
	// with a false "missing" notice.
	if snap.Output != nil && !snap.Output.Deleted && snap.Output.Status == "ready" && snap.Output.SnapshotHash == snap.Hash {
		return preparationOutput{}, false, 0, nil
	}
	status := "missing"
	title := "这份服务器准备缺少材料"
	why := "选中的材料已经找不到。补上材料或重新选择后，服务器会再准备。"
	if len(snap.FactIssues) > 0 {
		why = "选中的记录和它的来源对不上（来源内容已经变化）。请重新核对这条记录，或重新选择材料。"
	}
	if snap.Goal.Step == "已达成收尾" {
		status = "closed"
		title = "目标已经收尾"
		why = "目标收尾后不再生成新的准备。"
	}
	if snap.Output != nil && !snap.Output.Deleted && snap.Output.Status == status && snap.Output.SnapshotHash == snap.Hash {
		return preparationOutput{}, false, 0, nil
	}
	next := base
	if snap.Output == nil {
		next.Revision = 1
	} else {
		next.Revision = snap.Output.Revision + 1
	}
	next.Status = status
	next.XML = preparationNoticeXML(next.GoalID, status, title, why)
	next.Basis = nil
	next.Fingerprint = ""
	next.SnapshotHash = snap.Hash
	next.PolicyVersion = snap.Policy.Version
	next.Date = localNow.Format("2006-01-02")
	next.UpdatedAt = now.UTC().Format(time.RFC3339)
	next.Deleted = false
	return next, true, 0, nil
}

func (s *Store) runPreparationTickOne(user, clientKey string, now time.Time) (int, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.preparationSnapshot(user, clientKey)
		if err != nil {
			return 0, err
		}
		if err := preparationHook("after-snapshot"); err != nil {
			s.recordPreparationFailure(user, clientKey, now, err)
			return 0, err
		}
		next, hasNext, made, err := s.preparationNext(snap, now)
		if err != nil {
			return 0, err
		}
		if !hasNext {
			// 即使这次判定为 noop，也要在提交边界重读快照：来源可能在读取后
			// 变了，旧 ready 输出不能继续带着旧引文留在客户端。
			err := s.write(func(tx *sql.Tx) error {
				currentPolicy, err := readPreparationPolicyTx(tx, user, clientKey)
				if err != nil {
					return err
				}
				if currentPolicy.Version != snap.Policy.Version {
					return ErrConflict
				}
				currentSnap, err := preparationSnapshotTx(tx, user, clientKey)
				if err != nil {
					return err
				}
				revision := 0
				if snap.Output != nil {
					revision = snap.Output.Revision
				}
				if currentSnap.Hash == snap.Hash {
					return recordPreparationRunTx(tx, user, clientKey, "noop", revision, "", now.UTC().Format(time.RFC3339))
				}
				var currentOut preparationOutput
				outErr := readPreparationOutputTx(tx, user, clientKey, &currentOut)
				if outErr == nil {
					if currentOut.Deleted || currentOut.Status == "stale" {
						return recordPreparationRunTx(tx, user, clientKey, "noop", currentOut.Revision, "", now.UTC().Format(time.RFC3339))
					}
					currentOut.Revision++
					currentOut.Status = "stale"
					currentOut.XML = preparationNoticeXML(currentOut.GoalID, "stale", "这份服务器准备已失效", "依据已经撤回或发生变化，旧内容不再显示。")
					currentOut.Basis = nil
					currentOut.SnapshotHash = currentSnap.Hash
					currentOut.PolicyVersion = currentPolicy.Version
					currentOut.UpdatedAt = now.UTC().Format(time.RFC3339)
					if err := writePreparationOutputTx(tx, currentOut); err != nil {
						return err
					}
					return recordPreparationRunTx(tx, user, clientKey, "stale", currentOut.Revision, "", now.UTC().Format(time.RFC3339))
				}
				if !errors.Is(outErr, ErrNotFound) {
					return outErr
				}
				return recordPreparationRunTx(tx, user, clientKey, "noop", 0, "", now.UTC().Format(time.RFC3339))
			})
			if err != nil {
				if errors.Is(err, ErrConflict) {
					lastErr = err
					continue
				}
				s.recordPreparationFailure(user, clientKey, now, err)
				return 0, err
			}
			return 0, nil
		}
		err = s.write(func(tx *sql.Tx) error {
			currentPolicy, err := readPreparationPolicyTx(tx, user, clientKey)
			if err != nil {
				return err
			}
			if currentPolicy.Version != snap.Policy.Version {
				return ErrConflict
			}
			currentSnap, err := preparationSnapshotTx(tx, user, clientKey)
			if err != nil {
				return err
			}
			if currentSnap.Hash != snap.Hash {
				return ErrConflict
			}
			var currentOut preparationOutput
			outErr := readPreparationOutputTx(tx, user, clientKey, &currentOut)
			if outErr == nil {
				if snap.Output == nil || currentOut.Revision != snap.Output.Revision {
					return ErrConflict
				}
			} else if !errors.Is(outErr, ErrNotFound) {
				return outErr
			}
			if err := preparationHook("before-commit"); err != nil {
				return err
			}
			if err := writePreparationOutputTx(tx, next); err != nil {
				return err
			}
			return recordPreparationRunTx(tx, user, clientKey, next.Status, next.Revision, "", now.UTC().Format(time.RFC3339))
		})
		if err != nil {
			if errors.Is(err, ErrConflict) {
				lastErr = err
				continue
			}
			s.recordPreparationFailure(user, clientKey, now, err)
			return 0, err
		}
		if err := preparationHook("after-commit"); err != nil {
			s.recordPreparationFailure(user, clientKey, now, err)
			return 0, err
		}
		return made, nil
	}
	s.recordPreparationFailure(user, clientKey, now, lastErr)
	return 0, lastErr
}

// RunPreparationTick 只推进一个 owner 的准备；手动 tick 走这里。
func (s *Store) RunPreparationTick(user string, now time.Time) (int, error) {
	keys, err := s.preparationKeys(user)
	if err != nil {
		return 0, err
	}
	made := 0
	for _, key := range keys {
		n, err := s.runPreparationTickOne(user, key, now)
		if err != nil {
			return made, err
		}
		made += n
	}
	return made, nil
}

// RunPreparationTickAll 供服务端内部调度遍历所有用户；绝不接受请求体里的
// user 字段。
func (s *Store) RunPreparationTickAll(now time.Time) (int, error) {
	rows, err := s.db.Query("SELECT id FROM users ORDER BY rowid")
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
	rows.Close()
	made := 0
	for _, user := range users {
		n, err := s.RunPreparationTick(user, now)
		if err != nil {
			return made, err
		}
		made += n
	}
	return made, nil
}

func (s *Store) invalidatePreparationOne(user, clientKey string, now time.Time) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := s.preparationSnapshot(user, clientKey)
		if err != nil {
			return err
		}
		if snap.Output == nil || snap.Output.Deleted || snap.Output.Status == "stale" || snap.Output.SnapshotHash == snap.Hash {
			return nil
		}
		next := *snap.Output
		next.Revision++
		next.Status = "stale"
		next.XML = preparationNoticeXML(next.GoalID, "stale", "这份服务器准备已失效", "依据已经撤回或发生变化，旧内容不再显示。")
		next.Basis = nil
		next.SnapshotHash = snap.Hash
		next.PolicyVersion = snap.Policy.Version
		next.UpdatedAt = now.UTC().Format(time.RFC3339)
		err = s.write(func(tx *sql.Tx) error {
			currentPolicy, err := readPreparationPolicyTx(tx, user, clientKey)
			if err != nil {
				return err
			}
			if currentPolicy.Version != snap.Policy.Version {
				return ErrConflict
			}
			currentSnap, err := preparationSnapshotTx(tx, user, clientKey)
			if err != nil {
				return err
			}
			if currentSnap.Hash != snap.Hash {
				return ErrConflict
			}
			var out preparationOutput
			if err := readPreparationOutputTx(tx, user, clientKey, &out); err != nil {
				return err
			}
			if out.Revision != snap.Output.Revision {
				return ErrConflict
			}
			if err := writePreparationOutputTx(tx, next); err != nil {
				return err
			}
			return recordPreparationRunTx(tx, user, clientKey, "stale", next.Revision, "", now.UTC().Format(time.RFC3339))
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// refreshPreparationInvalidation 在读取 feed 时先做失效维护，撤回/更正后
// 不必等小时调度；暂停的准备同样维护。
func (s *Store) refreshPreparationInvalidation(user string, now time.Time) error {
	keys, err := s.preparationKeys(user)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := s.invalidatePreparationOne(user, key, now); err != nil {
			return err
		}
	}
	return nil
}

// PreparationFeed 返回 owner 自己的目标、可选材料、准备策略和输出。account
// 是稳定的 owner 命名空间；origin 由 HTTP handler 补上。这里不返回其他
// 用户的任何内容，也不把 Android 档案写回。
func (s *Store) PreparationFeed(user string, now time.Time) (object, error) {
	if err := s.refreshPreparationInvalidation(user, now); err != nil {
		return nil, err
	}
	goals, err := s.Goals(user)
	if err != nil {
		return nil, err
	}
	sources, err := s.Sources(user)
	if err != nil {
		return nil, err
	}
	facts, err := s.Portrait(user)
	if err != nil {
		return nil, err
	}
	goalRows := make([]any, 0, len(goals))
	for _, g := range goals {
		goalRows = append(goalRows, object{"id": g.ID, "title": g.Title, "step": g.Step, "unit": g.Unit})
	}
	materials := make([]any, 0, len(facts)+len(sources))
	for _, m := range facts {
		materials = append(materials, object{
			"kind": "fact", "id": m.ID, "title": truncate(m.Text, 80), "version": m.Version,
			"confirmed": m.Confirmed, "preview": truncate(m.Text, 160),
		})
	}
	for _, src := range sources {
		materials = append(materials, object{
			"kind": "source", "id": src.ID, "title": src.Title, "version": 1,
			"preview": truncate(src.Text, 160),
		})
	}
	rows, err := s.db.Query(`SELECT client_key,goal_id,enabled,version,timezone,request,selected_json,created_at,updated_at
		FROM preparation_policies WHERE user_id=? ORDER BY client_key`, user)
	if err != nil {
		return nil, err
	}
	policies := []preparationPolicy{}
	for rows.Next() {
		var p preparationPolicy
		var enabled int
		var selected string
		if err := rows.Scan(&p.ClientKey, &p.GoalID, &enabled, &p.Version, &p.Timezone, &p.Request, &selected, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		p.UserID = user
		p.Enabled = enabled != 0
		if err := json.Unmarshal([]byte(selected), &p.Selected); err != nil {
			rows.Close()
			return nil, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	preparations := []any{}
	for _, p := range policies {
		entry := object{
			"clientKey": p.ClientKey, "goalId": p.GoalID, "enabled": p.Enabled, "version": p.Version,
			"timezone": p.Timezone, "request": p.Request, "selected": preparationRefsAny(p.Selected),
			"createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt,
			"revision": 0, "status": "", "xml": "", "basis": []any{}, "date": "", "deleted": false,
		}
		out, outErr := scanPreparationOutput(s.db.QueryRow("SELECT "+preparationOutputColumns+" FROM preparation_outputs WHERE user_id=? AND client_key=?", user, p.ClientKey))
		if outErr == nil {
			entry["revision"] = out.Revision
			entry["status"] = out.Status
			entry["xml"] = out.XML
			entry["basis"] = toAnyBasis(out.Basis)
			entry["date"] = out.Date
			entry["deleted"] = out.Deleted
			entry["updatedAt"] = out.UpdatedAt
		} else if !errors.Is(outErr, ErrNotFound) {
			return nil, outErr
		}
		preparations = append(preparations, entry)
	}
	return object{
		"account":      preparationAccount(user),
		"goals":        goalRows,
		"materials":    materials,
		"preparations": preparations,
		"serverTime":   now.UTC().Format(time.RFC3339),
	}, nil
}
