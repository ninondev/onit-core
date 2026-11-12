package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 模型驱动的主动工作未实现前，tick 保持兼容但不编造建议卡。
func TestAgentTickIsCompatibleNoOp(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "agent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	if made, err := s.RunAgentTick(time.Now()); err != nil || made != 0 {
		t.Fatalf("tick made=%d err=%v", made, err)
	}
	if made, err := s.RunAgentTick(time.Now()); err != nil || made != 0 {
		t.Fatalf("second tick made=%d err=%v", made, err)
	}
	if made, err := s.RunAgentTickForUser(user, time.Now()); err != nil || made != 0 {
		t.Fatalf("owner tick made=%d err=%v", made, err)
	}
}

func TestAgentHTTPEndpoints(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "agent-http.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)
	user, _ := s.User(token)
	day := time.Now().Format("2006-01-02")

	// A fresh store answers the artifacts feed without a prior tick.
	raw := apiCall(t, server.URL, token, "GET", "/v1/artifacts/"+day, nil, 200)
	if strings.Contains(string(raw), "artifacts\":null") {
		t.Fatalf("artifacts should be an empty list: %s", raw)
	}
	if err = s.upsertArtifact(user, Artifact{ID: "widget:" + day + ":g1", Date: day, Type: "prepared", XML: "<prepared/>"}); err != nil {
		t.Fatal(err)
	}
	raw = apiCall(t, server.URL, token, "GET", "/v1/artifacts/"+day, nil, 200)
	if !strings.Contains(string(raw), "widget:"+day+":g1") {
		t.Fatalf("expected stored artifact: %s", raw)
	}
	// 他人隔离
	other := issue(t, s)
	raw2 := apiCall(t, server.URL, other, "GET", "/v1/artifacts/"+day, nil, 200)
	if strings.Contains(string(raw2), "widget:"+day+":g1") {
		t.Fatal("other user must not see artifacts")
	}
}
