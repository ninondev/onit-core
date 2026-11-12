package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Model addresses belong to the host configuration. A request can authorize
// a reading, but cannot redirect the service to a different machine.
type Model struct {
	Endpoint string `json:"endpoint"`
	Name     string `json:"model"`
}

type service struct {
	store      *Store
	model      Model
	client     *http.Client
	modelSlots chan struct{}
}
type userHandler func(http.ResponseWriter, *http.Request, string) error

type problem struct {
	status  int
	message string
}

func (p problem) Error() string { return p.message }

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])); ct != "application/json" {
		return problem{http.StatusUnsupportedMediaType, "content_type_json_required"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return problem{http.StatusRequestEntityTooLarge, "request_too_large"}
		}
		return problem{http.StatusBadRequest, "invalid_json"}
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return problem{http.StatusBadRequest, "expected_one_object"}
	}
	return nil
}

// requestOrigin 给客户端一个稳定的服务端命名空间。它只用于区分“哪个服务端
// 的哪张卡”，不参与授权；owner 仍只来自 Bearer token。
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); forwarded == "https" || forwarded == "http" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host
}

func failures(w http.ResponseWriter, err error) {
	p := problem{http.StatusInternalServerError, "storage_error"}
	switch {
	case errors.As(err, &p):
	case errors.Is(err, ErrNotFound):
		p = problem{http.StatusNotFound, "not_found"}
	case errors.Is(err, ErrConflict):
		p = problem{http.StatusConflict, "revision_changed"}
	case errors.Is(err, ErrVaultKeyMissing):
		p = problem{http.StatusConflict, "vault_key_missing"}
	case errors.Is(err, ErrVaultKeyInvalid):
		p = problem{http.StatusConflict, "vault_key_invalid"}
	case errors.Is(err, ErrVaultKeyConflict):
		p = problem{http.StatusConflict, "vault_key_conflict"}
	case errors.Is(err, ErrVaultKeyMismatch):
		p = problem{http.StatusConflict, "vault_key_mismatch"}
	case errors.Is(err, ErrVaultCredentialCorrupt):
		p = problem{http.StatusConflict, "vault_credential_corrupt"}
	case errors.Is(err, ErrVaultLegacyKeyUnverified):
		p = problem{http.StatusConflict, "vault_legacy_key_unverified"}
	case errors.Is(err, ErrInvalid):
		p = problem{http.StatusBadRequest, "invalid_input"}
	}
	respond(w, p.status, map[string]string{"error": p.message})
}

func newService(store *Store, model Model, identity ...selfhostIdentity) http.Handler {
	s := &service{store: store, model: model, client: &http.Client{Timeout: 90 * time.Second}, modelSlots: make(chan struct{}, 2)}
	selfhost := selfhostIdentity{}
	if len(identity) > 0 {
		selfhost = identity[0]
	}
	mux := http.NewServeMux()
	handle := func(pattern string, f userHandler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			raw := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(raw, "Bearer ")
			if !ok || len(token) < 24 || len(token) > 200 {
				failures(w, problem{401, "authentication_required"})
				return
			}
			user, err := store.User(token)
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
				failures(w, problem{401, "authentication_required"})
				return
			}
			if err != nil {
				failures(w, err)
				return
			}
			if err = f(w, r, user); err != nil {
				failures(w, err)
			}
		})
	}
	handle("GET /v1/model", func(w http.ResponseWriter, _ *http.Request, _ string) error {
		respond(w, 200, s.model)
		return nil
	})
	// 自部署身份：seed 用它确认 HTTP 服务确实在服务所选数据目录，
	// 而不是另一个恰好持有效凭据的普通库。只返回非秘密的目录身份。
	handle("GET /v1/selfhost", func(w http.ResponseWriter, _ *http.Request, _ string) error {
		respond(w, 200, selfhost)
		return nil
	})
	// 凭据金库（F-067）：客户端以用户金库公钥做 HPKE 封装后只上传密文；
	// 解封不提供 HTTP 入口，仅在使用现场（连接器取用）发生。
	handle("GET /v1/vault", func(w http.ResponseWriter, _ *http.Request, user string) error {
		public, err := store.VaultPublicKey(user)
		if err != nil {
			return err
		}
		respond(w, 200, object{"publicKey": base64.StdEncoding.EncodeToString(public), "suite": "HPKE/base/X25519+HKDF-SHA256+ChaCha20-Poly1305"})
		return nil
	})
	handle("POST /v1/vault/credentials", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct{ Label, Enc, Ct, Aad string }
		if err := decode(w, r, &in); err != nil {
			return err
		}
		enc, e1 := base64.StdEncoding.DecodeString(in.Enc)
		ct, e2 := base64.StdEncoding.DecodeString(in.Ct)
		aad, e3 := base64.StdEncoding.DecodeString(in.Aad)
		if e1 != nil || e2 != nil || e3 != nil {
			return ErrInvalid
		}
		id, err := store.StoreSealedCredential(user, in.Label, enc, ct, aad)
		if err != nil {
			return err
		}
		respond(w, 200, object{"id": id})
		return nil
	})
	// 原版 deleteAccount(connector:id:) 的连接凭据侧：客户端可删除某个
	// 连接账号存下的封装凭据（F-076 删除面）。
	handle("DELETE /v1/vault/credentials/{id}", func(w http.ResponseWriter, r *http.Request, user string) error {
		if err := store.DeleteSealedCredential(user, r.PathValue("id")); err != nil {
			return err
		}
		respond(w, 200, object{"deleted": true})
		return nil
	})
	handle("POST /v1/sources", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct{ Kind, Origin, Title, Text string }
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if in.Kind != "self" && in.Kind != "file" && in.Kind != "mail" {
			return ErrInvalid
		}
		if utf8.RuneCountInString(in.Origin) > 140 || !bounded(in.Title, 140) || !bounded(in.Text, 120000) {
			return ErrInvalid
		}
		source, err := store.importMaterial(user, in.Kind, in.Origin, in.Title, in.Text)
		if err != nil {
			return err
		}
		respond(w, 200, source)
		return nil
	})
	handle("GET /v1/sources", func(w http.ResponseWriter, _ *http.Request, user string) error {
		ss, err := store.Sources(user)
		if err != nil {
			return err
		}
		type sourceInfo struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Origin string `json:"origin,omitempty"`
			Title  string `json:"title"`
			At     string `json:"at,omitempty"`
		}
		rows := make([]sourceInfo, 0, len(ss))
		for _, src := range ss {
			rows = append(rows, sourceInfo{src.ID, src.Kind, src.Origin, src.Title, src.At})
		}
		respond(w, 200, rows)
		return nil
	})
	handle("GET /v1/sources/{id}", func(w http.ResponseWriter, r *http.Request, user string) error {
		src, err := store.Source(user, r.PathValue("id"))
		if err != nil {
			return err
		}
		respond(w, 200, src)
		return nil
	})
	handle("DELETE /v1/sources/{id}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			KeepConfirmed []string          `json:"keepConfirmed"`
			KeepReadings  []ReadingRevision `json:"keepReadings"`
		}
		if r.ContentLength != 0 {
			if err := decode(w, r, &in); err != nil {
				return err
			}
		}
		if err := store.RemoveSource(user, r.PathValue("id"), in.KeepConfirmed, in.KeepReadings...); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET /v1/portrait", func(w http.ResponseWriter, _ *http.Request, user string) error {
		rows, err := store.Portrait(user)
		if err != nil {
			return err
		}
		if rows == nil {
			rows = []Memory{}
		}
		respond(w, 200, rows)
		return nil
	})
	handle("POST /v1/memories", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Text     string         `json:"text"`
			Evidence []EvidenceSpan `json:"evidence"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		row, err := store.AddCrossClaim(user, in.Text, in.Evidence)
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("PATCH /v1/memories/{id}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Text    string
			Version int
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if !bounded(in.Text, 1000) || in.Version < 1 {
			return ErrInvalid
		}
		row, err := store.Correct(user, r.PathValue("id"), in.Text, in.Version)
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("DELETE /v1/memories/{id}", func(w http.ResponseWriter, r *http.Request, user string) error {
		if err := store.Forget(user, r.PathValue("id")); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})

	handle("GET /v1/discoveries", func(w http.ResponseWriter, _ *http.Request, user string) error {
		rows, err := store.Discoveries(user)
		if err != nil {
			return err
		}
		respond(w, 200, rows)
		return nil
	})
	handle("POST /v1/discoveries", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Title     string   `json:"title"`
			Body      string   `json:"body"`
			Kind      string   `json:"kind"`
			Citations []object `json:"citations"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		row, err := store.CreateDiscovery(user, in.Title, in.Body, in.Kind, in.Citations)
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("POST /v1/discoveries/{id}/correct", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Text    string `json:"text"`
			Version int    `json:"version"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		row, err := store.CorrectDiscovery(user, r.PathValue("id"), in.Text, in.Version)
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("GET /v1/proposals", func(w http.ResponseWriter, _ *http.Request, user string) error {
		rows, err := store.Proposals(user)
		if err != nil {
			return err
		}
		respond(w, 200, rows)
		return nil
	})
	handle("POST /v1/proposals", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Title     string          `json:"title"`
			Basis     string          `json:"basis"`
			BasisRefs *[]GoalBasisRef `json:"basisRefs"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		var row Proposal
		var err error
		if in.BasisRefs != nil {
			if in.Basis != "" {
				return ErrInvalid
			}
			row, err = store.ProposeWithRefs(user, in.Title, *in.BasisRefs)
		} else {
			row, err = store.Propose(user, in.Title, in.Basis)
		}
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("POST /v1/proposals/{id}/accept", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Title     string           `json:"title"`
			Step      string           `json:"step"`
			Unit      string           `json:"unit"`
			Basis     *ReadingRevision `json:"basis"`
			BasisRefs *[]GoalBasisRef  `json:"basisRefs"`
			Methods   []GoalMethod     `json:"methods"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		var row Goal
		var err error
		if in.BasisRefs != nil {
			if in.Basis != nil {
				return ErrInvalid
			}
			row, err = store.AcceptProposalWithRefs(user, r.PathValue("id"), in.Title, in.Step, in.Unit, *in.BasisRefs, in.Methods)
		} else {
			row, err = store.AcceptProposalWithBasis(user, r.PathValue("id"), in.Title, in.Step, in.Unit, in.Basis)
		}
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	// 服务端生成的卡片（F-050 artifacts 语义）：客户端今天页拉取合并。
	handle("GET /v1/artifacts/{date}", func(w http.ResponseWriter, r *http.Request, user string) error {
		rows, err := store.Artifacts(user, r.PathValue("date"))
		if err != nil {
			return err
		}
		if rows == nil {
			rows = []Artifact{}
		}
		respond(w, 200, object{"artifacts": rows})
		return nil
	})
	// 手动触发一轮 agent（调试/演示用；正式由 StartAgentScheduler 周期跑）。
	// 只跑当前认证 owner，不能借手动 tick 推进其他用户。
	handle("POST /v1/agent/tick", func(w http.ResponseWriter, _ *http.Request, user string) error {
		made, err := store.RunAgentTickForUser(user, time.Now())
		if err != nil {
			return err
		}
		preparationMade, err := store.RunPreparationTick(user, time.Now())
		if err != nil {
			return err
		}
		respond(w, 200, object{"made": made, "preparationMade": preparationMade})
		return nil
	})
	// 自部署主动准备：owner 自己的策略、材料和输出。origin 与 account 组成
	// 客户端命名空间，服务端来源不能用本地同名 id 解析。
	handle("GET /v1/preparation", func(w http.ResponseWriter, r *http.Request, user string) error {
		feed, err := store.PreparationFeed(user, time.Now())
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("PUT /v1/preparation/{clientKey}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			GoalID   string           `json:"goalId"`
			Enabled  bool             `json:"enabled"`
			Timezone string           `json:"timezone"`
			Request  string           `json:"request"`
			Selected []preparationRef `json:"selected"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		policy, err := store.SavePreparationPolicy(user, r.PathValue("clientKey"), in.GoalID, in.Enabled, in.Timezone, in.Request, in.Selected)
		if err != nil {
			return err
		}
		respond(w, 200, policy)
		return nil
	})
	handle("DELETE /v1/preparation/{clientKey}", func(w http.ResponseWriter, r *http.Request, user string) error {
		if err := store.ClearPreparation(user, r.PathValue("clientKey")); err != nil {
			return err
		}
		respond(w, 200, object{"ok": true})
		return nil
	})
	handle("POST /v1/preparation/tick", func(w http.ResponseWriter, r *http.Request, user string) error {
		made, err := store.RunPreparationTick(user, time.Now())
		if err != nil {
			return err
		}
		feed, err := store.PreparationFeed(user, time.Now())
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["made"] = made
		respond(w, 200, feed)
		return nil
	})
	// R40 持续目标复盘：owner 只来自 Bearer token。开启、暂停、反馈、继续、
	// 取消都是正式命令；手动 tick 只推进当前 owner，内部 scheduler 才遍历。
	handle("GET /v1/goal-review", func(w http.ResponseWriter, r *http.Request, user string) error {
		feeds, err := store.GoalReviewFeeds(user, requestOrigin(r))
		if err != nil {
			return err
		}
		respond(w, 200, object{"follows": feeds, "origin": requestOrigin(r), "account": preparationAccount(user)})
		return nil
	})
	handle("GET /v1/goal-review/{goalId}", func(w http.ResponseWriter, r *http.Request, user string) error {
		feed, err := store.GoalReviewFeed(user, r.PathValue("goalId"), requestOrigin(r))
		if err != nil {
			return err
		}
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("PUT /v1/goal-review/{goalId}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in goalReviewFollowInput
		if err := decode(w, r, &in); err != nil {
			return err
		}
		feed, err := store.SaveGoalReviewFollow(user, r.PathValue("goalId"), in)
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("DELETE /v1/goal-review/{goalId}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			RequestID               string `json:"requestId"`
			ExpectedControlRevision *int   `json:"expectedControlRevision"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if err := store.DeleteGoalReviewFollow(user, r.PathValue("goalId"), in.RequestID, in.ExpectedControlRevision); err != nil {
			return err
		}
		respond(w, 200, object{"ok": true})
		return nil
	})
	handle("POST /v1/goal-review/{goalId}/feedback", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in goalReviewFeedbackInput
		if err := decode(w, r, &in); err != nil {
			return err
		}
		feed, err := store.GoalReviewFeedback(user, r.PathValue("goalId"), in)
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("POST /v1/goal-review/{goalId}/continue", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			RequestID               string `json:"requestId"`
			ExpectedControlRevision *int   `json:"expectedControlRevision"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		feed, err := store.ContinueGoalReview(user, r.PathValue("goalId"), in.RequestID, in.ExpectedControlRevision)
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("POST /v1/goal-review/{goalId}/cancel", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			RequestID               string `json:"requestId"`
			ExpectedControlRevision *int   `json:"expectedControlRevision"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		feed, err := store.CancelGoalReview(user, r.PathValue("goalId"), in.RequestID, in.ExpectedControlRevision)
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("POST /v1/goal-review/{goalId}/seen", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			AttentionRevision int `json:"attentionRevision"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		feed, err := store.MarkGoalReviewSeen(user, r.PathValue("goalId"), in.AttentionRevision)
		if err != nil {
			return err
		}
		feed["origin"] = requestOrigin(r)
		feed["account"] = preparationAccount(user)
		respond(w, 200, feed)
		return nil
	})
	handle("POST /v1/goal-review/tick", func(w http.ResponseWriter, r *http.Request, user string) error {
		made, err := store.RunGoalReviewTick(user, goalReviewNow())
		if err != nil {
			return err
		}
		feeds, err := store.GoalReviewFeeds(user, requestOrigin(r))
		if err != nil {
			return err
		}
		respond(w, 200, object{"made": made, "follows": feeds, "origin": requestOrigin(r), "account": preparationAccount(user)})
		return nil
	})
	// 事件驱动 agent：客户端推送领域事件，agent 决定是否生成消息。
	handle("POST /v1/events", func(w http.ResponseWriter, r *http.Request, user string) error {
		var ev Event
		if err := decode(w, r, &ev); err != nil {
			return err
		}
		if ev.Type == "" || len(ev.Data) == 0 || len(ev.Data) > 16384 {
			return ErrInvalid
		}
		reply, err := store.HandleEvent(user, ev)
		if err != nil {
			return err
		}
		respond(w, 200, reply)
		return nil
	})

	// 多模型编排：代理调用多家模型并汇总。
	handle("POST /v1/orchestrate", func(w http.ResponseWriter, r *http.Request, user string) error {
		var req OrchestrateRequest
		if err := decode(w, r, &req); err != nil {
			return err
		}
		result, err := Orchestrate(req)
		if err != nil {
			return err
		}
		respond(w, 200, result)
		return nil
	})

	handle("GET /v1/goals", func(w http.ResponseWriter, _ *http.Request, user string) error {
		rows, err := store.Goals(user)
		if err != nil {
			return err
		}
		respond(w, 200, rows)
		return nil
	})
	handle("POST /v1/goals/{id}/entries", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Amount float64 `json:"amount"`
			Note   string  `json:"note"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		row, err := store.RecordGoal(user, r.PathValue("id"), in.Amount, in.Note)
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("PATCH /v1/goals/{id}/entries/{entryId}", func(w http.ResponseWriter, r *http.Request, user string) error {
		var in struct {
			Amount *float64 `json:"amount"`
			Note   *string  `json:"note"`
		}
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if err := store.ChangeGoalEntry(user, r.PathValue("id"), r.PathValue("entryId"), in.Amount, in.Note); err != nil {
			return err
		}
		w.WriteHeader(204)
		return nil
	})
	handle("GET /v1/reports", func(w http.ResponseWriter, _ *http.Request, user string) error {
		rows, err := store.Reports(user)
		if err != nil {
			return err
		}
		respond(w, 200, rows)
		return nil
	})
	handle("POST /v1/reports/week", func(w http.ResponseWriter, _ *http.Request, user string) error {
		row, err := store.WeeklyReport(user, time.Now().UTC())
		if err != nil {
			return err
		}
		respond(w, 200, row)
		return nil
	})
	handle("POST /v1/sources/{id}/read", s.modelRead)
	return mux
}

func bounded(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}

func (s *service) modelRead(w http.ResponseWriter, r *http.Request, user string) error {
	var in struct {
		Model
		SelectedText string `json:"selectedText"`
		Key          string `json:"apiKey"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	src, err := s.store.Source(user, r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.model.Endpoint == "" || s.model.Name == "" {
		return problem{409, "model_not_configured"}
	}
	if in.Model != s.model {
		return problem{409, "model_destination_changed"}
	}
	if len(in.SelectedText) > maxSelectedBytes || !bounded(in.SelectedText, 24000) || !strings.Contains(src.Text, in.SelectedText) || len(in.Key) > 4096 || strings.ContainsAny(in.Key, "\r\n") {
		return ErrInvalid
	}
	select {
	case s.modelSlots <- struct{}{}:
		defer func() { <-s.modelSlots }()
	default:
		return problem{429, "model_busy"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	findings, err := ReadWithModel(ctx, s.client, s.model.Endpoint, s.model.Name, in.Key, src.Kind, in.SelectedText)
	if err != nil {
		return problem{502, "model_read_failed"}
	}
	if len(findings) == 0 {
		return problem{422, "no_supported_observation"}
	}
	if err = s.store.AddReadings(user, src.ID, findings); err != nil {
		return err
	}
	respond(w, 200, map[string]bool{"read": true})
	return nil
}
