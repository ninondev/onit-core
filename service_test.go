package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func apiCall(t *testing.T, url, token, method, path string, in any, want int) []byte {
	t.Helper()
	payload, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, url+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s status %d (expected %d): %q", method, path, resp.StatusCode, want, body)
	}
	return body
}
func unpack[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func issue(t *testing.T, store *Store) string {
	t.Helper()
	token, err := store.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAdviserUserJourney(t *testing.T) {
	file := filepath.Join(t.TempDir(), "adviser.sqlite")
	open := func() (*Store, *httptest.Server) {
		db, err := OpenStore(file)
		if err != nil {
			t.Fatal(err)
		}
		return db, httptest.NewServer(newService(db, Model{}))
	}
	store, server := open()
	alice, bob := issue(t, store), issue(t, store)
	request := func(user, method, path string, input any, want int) []byte {
		return apiCall(t, server.URL, user, method, path, input, want)
	}
	request("", "GET", "/v1/portrait", nil, 401)
	material := map[string]string{"kind": "self", "title": "练习手记", "text": "我每周都练习大提琴，打算参加室内乐演出。"}
	src := unpack[Source](t, request(alice, "POST", "/v1/sources", material, 200))
	if got := unpack[[]Memory](t, request(alice, "GET", "/v1/portrait", nil, 200)); len(got) != 0 {
		t.Fatal("import extracted memories without an authorized read")
	}
	// 本人授权的模型阅读把这句原文带入记忆。
	aliceUser, err := store.User(alice)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AddReadings(aliceUser, src.ID, quotesToFindings([]string{material["text"]})); err != nil {
		t.Fatal(err)
	}
	facts := unpack[[]Memory](t, request(alice, "GET", "/v1/portrait", nil, 200))
	if len(facts) != 1 || !strings.Contains(material["text"], facts[0].Quote) {
		t.Fatalf("missing source-grounded observation: %v", facts)
	}
	other := unpack[[]Memory](t, request(bob, "GET", "/v1/portrait", nil, 200))
	if len(other) != 0 {
		t.Fatal("new account inherited a portrait")
	}
	request(bob, "GET", "/v1/sources/"+src.ID, nil, 404)
	request(bob, "PATCH", "/v1/memories/"+facts[0].ID, map[string]any{"text": "覆盖", "version": 1}, 404)
	request(bob, "DELETE", "/v1/memories/"+facts[0].ID, nil, 404)
	request(bob, "DELETE", "/v1/sources/"+src.ID, map[string]any{}, 404)
	const step = "每天练五分钟长弓"
	proposal := unpack[Proposal](t, request(alice, "POST", "/v1/proposals", map[string]string{"title": "练琴", "basis": facts[0].ID}, 200))
	accept := "/v1/proposals/" + proposal.ID + "/accept"
	choice := map[string]any{"title": "练琴", "step": step, "unit": "分钟", "basis": ReadingRevision{facts[0].ID, facts[0].Version}}
	request(bob, "POST", accept, choice, 404)
	choice["basis"] = ReadingRevision{"a-different-memory", facts[0].Version}
	request(alice, "POST", accept, choice, 409)
	choice["basis"] = ReadingRevision{facts[0].ID, facts[0].Version + 1}
	request(alice, "POST", accept, choice, 409)
	choice["basis"] = ReadingRevision{facts[0].ID, facts[0].Version}
	goal := unpack[Goal](t, request(alice, "POST", accept, choice, 200))
	initialBasis := GoalBasis{ID: facts[0].ID, AcceptedVersion: facts[0].Version, CurrentVersion: facts[0].Version, CurrentText: facts[0].Text}
	if goal.ProposalID != proposal.ID || goal.Step != step || goal.Basis == nil || *goal.Basis != initialBasis {
		t.Fatalf("accepted choice: %+v, basis %+v", goal, goal.Basis)
	}
	fixed := "我练大提琴是为了给外婆演奏，不准备登台。"
	patch := map[string]any{"text": fixed, "version": facts[0].Version}
	request(alice, "PATCH", "/v1/memories/"+facts[0].ID, patch, 200)
	request(alice, "PATCH", "/v1/memories/"+facts[0].ID, patch, 409)
	request(alice, "POST", "/v1/sources", material, 200)
	bobMaterial := map[string]string{"kind": "self", "title": "练习手记", "text": "我喜欢徒步，正在准备第一次过夜露营。"}
	request(bob, "POST", "/v1/sources", bobMaterial, 200)
	server.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, server = open()
	defer server.Close()
	defer store.Close()
	otherAfter := unpack[[]Memory](t, request(bob, "GET", "/v1/portrait", nil, 200))
	if len(otherAfter) != 0 {
		t.Fatal("portrait crossed a user boundary")
	}
	after := unpack[[]Memory](t, request(alice, "GET", "/v1/portrait", nil, 200))
	if len(after) != 1 || after[0].Text != fixed || !after[0].Confirmed || after[0].Quote != facts[0].Quote {
		t.Fatal("source, correction or confirmation lost")
	}
	checkGoal := func(currentVersion int, currentText string) {
		t.Helper()
		got := unpack[[]Goal](t, request(alice, "GET", "/v1/goals", nil, 200))
		want := GoalBasis{ID: facts[0].ID, AcceptedVersion: facts[0].Version, CurrentVersion: currentVersion, CurrentText: currentText}
		if len(got) != 1 || got[0].ID != goal.ID || got[0].ProposalID != proposal.ID || got[0].Step != step || got[0].Basis == nil || *got[0].Basis != want {
			t.Fatalf("expected the same goal and action with basis %+v; got %+v", want, got)
		}
	}
	checkGoal(after[0].Version, fixed)
	if otherGoals := unpack[[]Goal](t, request(bob, "GET", "/v1/goals", nil, 200)); len(otherGoals) != 0 {
		t.Fatalf("goal crossed a user boundary: %+v", otherGoals)
	}
	request(alice, "DELETE", "/v1/sources/"+src.ID, map[string]any{"keepConfirmed": []string{after[0].ID}}, 204)
	retained := unpack[[]Memory](t, request(alice, "GET", "/v1/portrait", nil, 200))
	if len(retained) != 1 || retained[0].SourceID != "" || retained[0].Quote != "" || retained[0].Text != fixed {
		t.Fatal("retained memory kept revoked evidence")
	}
	checkGoal(retained[0].Version, fixed)
	request(alice, "DELETE", "/v1/memories/"+after[0].ID, nil, 204)
	checkGoal(0, "")
}

func TestModelLeavesDeviceOnlyForAuthorizedExcerpt(t *testing.T) {
	const excerpt = "I prefer working in a quiet studio."
	const private = "I am planning a surprise for Julia."
	var mu sync.Mutex
	seen := []string{}
	modelHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		mu.Unlock()
		reply, _ := json.Marshal(map[string]any{"findings": []Finding{{Text: "Quiet is an important working condition for you.", Quote: excerpt}}})
		respond(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(reply)}}}})
	}))
	defer modelHost.Close()
	cfg := Model{Endpoint: modelHost.URL, Name: "user-selected"}
	db, err := OpenStore(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(newService(db, cfg))
	defer server.Close()
	token, other := issue(t, db), issue(t, db)
	source := unpack[Source](t, apiCall(t, server.URL, token, "POST", "/v1/sources", map[string]string{"title": "Studio", "kind": "self", "text": excerpt + " " + private}, 200))
	path := "/v1/sources/" + source.ID + "/read"
	selectText := map[string]string{"endpoint": cfg.Endpoint, "model": cfg.Name, "selectedText": excerpt}
	apiCall(t, server.URL, other, "POST", path, selectText, 404)
	selectText["endpoint"] = cfg.Endpoint + "/other"
	apiCall(t, server.URL, token, "POST", path, selectText, 409)
	selectText["endpoint"] = cfg.Endpoint
	selectText["selectedText"] = "never appeared"
	apiCall(t, server.URL, token, "POST", path, selectText, 400)
	selectText["selectedText"] = excerpt
	apiCall(t, server.URL, token, "POST", path, selectText, 200)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || strings.Contains(seen[0], private) || !strings.Contains(seen[0], excerpt) {
		t.Fatalf("unexpected model traffic: %d", len(seen))
	}
}

func TestDisconnectExplicitReadingOverHTTP(t *testing.T) {
	db, err := OpenStore(filepath.Join(t.TempDir(), "retention.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(newService(db, Model{}))
	defer server.Close()
	user, token := storeUser(t, db)
	call := func(method, path string, data any, status int) []byte {
		return apiCall(t, server.URL, token, method, path, data, status)
	}
	const keepQuote = "我每周六都到社区排练。"
	const confirmQuote = "我打算去旅行。"
	const removeQuote = "我喜欢下雨天。"
	src := unpack[Source](t, call("POST", "/v1/sources", map[string]string{"kind": "mail", "title": "社团来信", "text": keepQuote + confirmQuote + removeQuote}, 200))
	mailReadings := []Finding{
		{Text: evidenceText("mail", keepQuote), Quote: keepQuote},
		{Text: evidenceText("mail", confirmQuote), Quote: confirmQuote},
		{Text: evidenceText("mail", removeQuote), Quote: removeQuote},
	}
	if err := db.AddReadings(user, src.ID, mailReadings); err != nil {
		t.Fatal(err)
	}
	portrait := func() map[string]Memory {
		out := map[string]Memory{}
		for _, m := range unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200)) {
			out[m.Quote] = m
		}
		return out
	}
	before := portrait()
	keep, confirm := before[keepQuote], before[confirmQuote]
	call("PATCH", "/v1/memories/"+confirm.ID, map[string]any{"text": "收信人只想留在本地。", "version": confirm.Version}, 200)
	if err := db.AddReadings(user, src.ID, []Finding{{Text: "另一种尚未采用的说法", Quote: keepQuote}}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/sources/" + src.ID
	chosen := map[string]any{"keepConfirmed": []string{confirm.ID}, "keepReadings": []ReadingRevision{{ID: keep.ID, Version: keep.Version}}}
	call("DELETE", path, chosen, 409)
	call("GET", path, nil, 200)
	keep = portrait()[keepQuote]
	if len(keep.Alternatives) != 1 {
		t.Fatal("missing alternative before disconnection")
	}
	chosen["keepReadings"] = []ReadingRevision{{ID: keep.ID, Version: keep.Version}, {ID: "unavailable", Version: 1}}
	call("DELETE", path, chosen, 409)
	if uncommitted := portrait()[keepQuote]; uncommitted.SourceID != src.ID || uncommitted.Version != keep.Version {
		t.Fatalf("part of failed request committed: %+v", uncommitted)
	}
	chosen["keepReadings"] = []ReadingRevision{{ID: keep.ID, Version: keep.Version}}
	apiCall(t, server.URL, issue(t, db), "DELETE", path, chosen, 404)
	call("DELETE", path, chosen, 204)
	call("GET", path, nil, 404)
	all := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
	if len(all) != 2 {
		t.Fatalf("unexpected retained memories: %+v", all)
	}
	for _, m := range all {
		if m.SourceID != "" || m.Quote != "" || len(m.Evidence) != 0 || len(m.Alternatives) != 0 {
			t.Fatalf("old evidence: %+v", m)
		}
		switch m.ID {
		case keep.ID:
			if m.Confirmed || !m.RetainedReading || m.Attribution != "mail" || m.Text != evidenceText("mail", keepQuote) || m.Version != keep.Version+1 {
				t.Fatalf("sender's reading: %+v", m)
			}
		case confirm.ID:
			if !m.Confirmed || m.RetainedReading || m.Attribution != "confirmed" || m.Text != "收信人只想留在本地。" {
				t.Fatalf("user correction: %+v", m)
			}
		default:
			t.Fatalf("unselected memory retained: %+v", m)
		}
		var history string
		if err := db.db.QueryRow("SELECT history FROM memories WHERE id=?", m.ID).Scan(&history); err != nil || history != "[]" {
			t.Fatalf("history not cleared: %v", err)
		}
	}
	retained := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
	keepRow := Memory{}
	for _, m := range retained {
		if m.ID == keep.ID && !m.RetainedReading {
			t.Fatalf("recall lost retention state: %+v", m)
		}
		if m.ID == keep.ID {
			keepRow = m
		}
	}
	if keepRow.ID == "" {
		t.Fatal("retained reading disappeared")
	}
	corrected := unpack[Memory](t, call("PATCH", "/v1/memories/"+keep.ID, map[string]any{"text": "只是社团来信提到过这件事。", "version": keep.Version + 1}, 200))
	if corrected.RetainedReading || !corrected.Confirmed {
		t.Fatal("confirmation did not supersede retained reading")
	}
}

func TestDisconnectPromotesOtherEvidenceBeforeDetaching(t *testing.T) {
	db, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(newService(db, Model{}))
	defer server.Close()
	token := issue(t, db)
	call := func(method, path string, data any, status int) []byte {
		return apiCall(t, server.URL, token, method, path, data, status)
	}
	source := func(title, text string) Source {
		return unpack[Source](t, call("POST", "/v1/sources", map[string]string{"kind": "self", "title": title, "text": text}, 200))
	}
	a, b := source("周六手记", "我每周六都练习画速写。"), source("出门清单", "我喜欢带着铅笔散步，边走边画速写。")
	row := unpack[Memory](t, call("POST", "/v1/memories", map[string]any{"text": "我在户外练习速写。", "evidence": []EvidenceSpan{{SourceID: a.ID, Quote: a.Text}, {SourceID: b.ID, Quote: b.Text}}}, 200))
	call("DELETE", "/v1/sources/"+a.ID, map[string]any{"keepReadings": []ReadingRevision{{ID: row.ID, Version: row.Version}}}, 204)
	remaining := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
	for _, m := range remaining {
		if m.ID != row.ID {
			continue
		}
		if m.SourceID != b.ID || m.Quote != b.Text || m.Version != row.Version+1 || len(m.Evidence) != 1 || m.Evidence[0].SourceID != b.ID || m.RetainedReading || m.Confirmed {
			t.Fatalf("surviving source lost: %+v", m)
		}
		call("DELETE", "/v1/sources/"+b.ID, map[string]any{"keepReadings": []ReadingRevision{{ID: m.ID, Version: m.Version}}}, 204)
		detached := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))
		if len(detached) != 1 || detached[0].Text != row.Text || detached[0].Attribution != "self" || !detached[0].RetainedReading || len(detached[0].Evidence) != 0 {
			t.Fatalf("explicit final-source retention lost current reading: %+v", detached)
		}
		return
	}
	t.Fatal("cross-source memory disappeared")
}

func TestDisconnectRefusesPrivateOrSupersededReadings(t *testing.T) {
	db, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(newService(db, Model{}))
	defer server.Close()
	run := func(t *testing.T, token, kind, body, quote, claim string) {
		call := func(method, path string, data any, status int) []byte {
			return apiCall(t, server.URL, token, method, path, data, status)
		}
		src := unpack[Source](t, call("POST", "/v1/sources", map[string]string{"kind": kind, "title": "测试资料", "text": body}, 200))
		row := unpack[Memory](t, call("POST", "/v1/memories", map[string]any{"text": claim, "evidence": []EvidenceSpan{{SourceID: src.ID, Quote: quote}}}, 200))
		path := "/v1/sources/" + src.ID
		call("DELETE", path, map[string]any{"keepReadings": []ReadingRevision{{ID: row.ID, Version: row.Version}}}, 409)
		call("GET", path, nil, 200)
		call("DELETE", path, nil, 204)
	}
	const quote = "我希望能学会修理瓷器。"
	for _, tc := range []struct{ name, kind, body, evidence, claim string }{
		{"request-for-privacy", "self", "只聊这一次，别记进画像。" + quote, quote, quote},
		{"credential-outside-short-quote", "self", "我最喜欢的密钥是 ABC。", "ABC", "我的偏好是 ABC。"},
		{"forwarded-mail", "mail", "一封转寄。\n---- Original Message ----\n" + quote, quote, evidenceText("mail", quote)},
		{"wrong-speaker", "mail", quote, quote, "收信人期待学会修理瓷器。"},
		{"dropped-condition", "self", "我希望能学会修理瓷器，但目前还没下决定。", "我希望能学会修理瓷器", "我确定会学修理瓷器。"},
	} {
		t.Run(tc.name, func(t *testing.T) { run(t, issue(t, db), tc.kind, tc.body, tc.evidence, tc.claim) })
	}
	authorizedRead := func(t *testing.T, token, sourceID string, quotes ...string) {
		t.Helper()
		user, err := db.User(token)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.AddReadings(user, sourceID, quotesToFindings(quotes)); err != nil {
			t.Fatal(err)
		}
	}
	for _, action := range []string{"correct", "forget"} {
		t.Run(action+"-duplicate", func(t *testing.T) {
			token := issue(t, db)
			src := unpack[Source](t, apiCall(t, server.URL, token, "POST", "/v1/sources", map[string]string{"kind": "self", "title": "旧资料", "text": quote}, 200))
			authorizedRead(t, token, src.ID, quote)
			rows := unpack[[]Memory](t, apiCall(t, server.URL, token, "GET", "/v1/portrait", nil, 200))
			if len(rows) == 0 {
				t.Fatal("authorized reading missing")
			}
			if action == "correct" {
				apiCall(t, server.URL, token, "PATCH", "/v1/memories/"+rows[0].ID, map[string]any{"text": "我想学的是修理旧灯。", "version": rows[0].Version}, 200)
			} else {
				apiCall(t, server.URL, token, "DELETE", "/v1/memories/"+rows[0].ID, nil, 204)
			}
			run(t, token, "self", quote, quote, quote)
		})
	}
	t.Run("overlap-in-same-source", func(t *testing.T) {
		token := issue(t, db)
		call := func(method, path string, data any, status int) []byte {
			return apiCall(t, server.URL, token, method, path, data, status)
		}
		src := unpack[Source](t, call("POST", "/v1/sources", map[string]string{"kind": "self", "title": "手记", "text": quote}, 200))
		authorizedRead(t, token, src.ID, quote)
		first := unpack[[]Memory](t, call("GET", "/v1/portrait", nil, 200))[0]
		part := unpack[Memory](t, call("POST", "/v1/memories", map[string]any{"text": "修理瓷器", "evidence": []EvidenceSpan{{SourceID: src.ID, Quote: "修理瓷器"}}}, 200))
		call("DELETE", "/v1/memories/"+part.ID, nil, 204)
		call("DELETE", "/v1/sources/"+src.ID, map[string]any{"keepReadings": []ReadingRevision{{ID: first.ID, Version: first.Version}}}, 409)
		call("GET", "/v1/sources/"+src.ID, nil, 200)
	})
}

// A reply can quote the same words as its author. The first occurrence belongs
// to the quoted thread; storing that occurrence would point to the wrong speaker.
func TestAuthorizedReadingEvidenceUsesAuthoredOccurrence(t *testing.T) {
	const quote = "我星期二去学游泳。"
	const body = "> " + quote + "\n🦉\n" + quote
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(newService(store, Model{}))
	defer server.Close()
	token := issue(t, store)
	user, err := store.User(token)
	if err != nil {
		t.Fatal(err)
	}
	src := unpack[Source](t, apiCall(t, server.URL, token, "POST", "/v1/sources", map[string]string{"kind": "self", "title": "泳池", "text": body}, 200))
	if err = store.AddReadings(user, src.ID, quotesToFindings([]string{quote})); err != nil {
		t.Fatal(err)
	}
	got := unpack[[]Memory](t, apiCall(t, server.URL, token, "GET", "/v1/portrait", nil, 200))
	at := strings.LastIndex(body, quote)
	if len(got) != 1 || len(got[0].Evidence) != 1 {
		t.Fatal(got)
	}
	span := got[0].Evidence[0]
	if span.Quote != quote || span.Start != at || span.End != at+len(quote) {
		t.Fatalf("wrong stored location: %+v", span)
	}
}
