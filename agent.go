package main

// Compatibility endpoint for legacy agent ticks. Model-backed proactive work
// is not implemented here; ticks do not invent advice or prune user history.
import (
	"database/sql"
	"strings"
	"time"
)

const artifactSchema = `
CREATE TABLE IF NOT EXISTS artifacts (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
 date TEXT NOT NULL, type TEXT NOT NULL, xml TEXT NOT NULL, at TEXT NOT NULL
);`

type Artifact struct {
	ID   string `json:"id"`
	Date string `json:"date"`
	Type string `json:"type"`
	XML  string `json:"xml"`
}

// nudgeCard 用与 nudge.xml 模板相同的字段词汇（badge/message/sent-at）。
// xmlEscape 转义 XML 文本内容（目标标题等用户输入）。
func xmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return replacer.Replace(s)
}

func (s *Store) upsertArtifact(user string, a Artifact) error {
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(artifactSchema); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO artifacts(id,user_id,date,type,xml,at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET xml=excluded.xml, at=excluded.at
			WHERE artifacts.user_id = excluded.user_id`,
			a.ID, user, a.Date, a.Type, a.XML, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		// 全局 artifact id 冲突时，另一个 owner 的行不能被覆盖。RowsAffected
		// 为 0 表示冲突行的 user_id 不同；跳过这张卡，不把它写进别人的空间。
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConflict
		}
		return nil
	})
}

// RunAgentTick 跑一轮：为每个用户的活跃目标生成当日 nudge 卡（最近记录
// >=1 天且未停用），然后清理（每用户保留最近 500 张）。
func (s *Store) RunAgentTick(now time.Time) (int, error) {
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
		n, err := s.runAgentTickForUser(user, now)
		if err != nil {
			return made, err
		}
		made += n
	}
	return made, nil
}

// RunAgentTickForUser 只跑当前认证 owner；手动 tick 不再遍历所有用户。
func (s *Store) RunAgentTickForUser(user string, now time.Time) (int, error) {
	return s.runAgentTickForUser(user, now)
}

// No model-backed proactive decision is implemented here. Keep the endpoint
// compatible without fabricating advice or deleting historical user records.
func (s *Store) runAgentTickForUser(user string, now time.Time) (int, error) {
	return 0, nil
}

// goalReviewManaged reports whether this owner+goal is under the continuous
// review gate. The follow row is kept when the follow is paused, so pausing
// also stops the old daily nudge instead of letting it bypass the gate.
func (s *Store) goalReviewManaged(user, goalID string) bool {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM goal_review_follows WHERE user_id=? AND goal_id=? AND deleted=0`,
		user, goalID).Scan(&exists); err != nil {
		return false
	}
	return exists > 0
}

// Artifacts 按日期取卡（客户端今天页合并用）。已纳入持续复盘的目标，旧
// nudge 卡仍保留在 artifacts 历史里，但不再投影给客户端；不删除整套记录。
func (s *Store) Artifacts(user, date string) ([]Artifact, error) {
	managed := map[string]bool{}
	mrows, err := s.db.Query(`SELECT goal_id FROM goal_review_follows WHERE user_id=? AND deleted=0`, user)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var id string
		if err := mrows.Scan(&id); err != nil {
			mrows.Close()
			return nil, err
		}
		managed[id] = true
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return nil, err
	}
	mrows.Close()
	rows, err := s.db.Query(`SELECT id,date,type,xml FROM artifacts WHERE user_id=? AND date=? ORDER BY at`, user, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.Date, &a.Type, &a.XML); err != nil {
			return nil, err
		}
		if a.Type == "nudge" {
			prefix := "widget:" + a.Date + ":"
			if strings.HasPrefix(a.ID, prefix) && managed[strings.TrimPrefix(a.ID, prefix)] {
				continue
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
