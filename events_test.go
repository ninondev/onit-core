package main

import (
	"encoding/json"
	"time"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventCalendarChanged(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ev.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)
	user, _ := s.User(token)

	data, _ := json.Marshal(CalendarEventData{Title: "团队会议", Kind: "moved", When: time.Now()})
	in := map[string]interface{}{"type": "calendar_changed", "data": json.RawMessage(data)}
	raw := apiCall(t, server.URL, token, "POST", "/v1/events", in, 200)
	if !strings.Contains(string(raw), "团队会议") || !strings.Contains(string(raw), "改期") {
		t.Fatalf("expected calendar message, got %s", raw)
	}
	_ = user
}

func TestEventGoalMilestone(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ev2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)

	data, _ := json.Marshal(GoalEventData{GoalID: "g1", Title: "跑步", Amount: 5, Unit: "公里", Total: 100})
	in := map[string]interface{}{"type": "goal_milestone", "data": json.RawMessage(data)}
	raw := apiCall(t, server.URL, token, "POST", "/v1/events", in, 200)
	if !strings.Contains(string(raw), "跑步") || !strings.Contains(string(raw), "100") {
		t.Fatalf("expected milestone message, got %s", raw)
	}
}

func TestEventGoalStale(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ev3.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)

	data, _ := json.Marshal(GoalEventData{GoalID: "g2", Title: "读书"})
	in := map[string]interface{}{"type": "goal_stale", "data": json.RawMessage(data)}
	raw := apiCall(t, server.URL, token, "POST", "/v1/events", in, 200)
	if !strings.Contains(string(raw), "没动静") {
		t.Fatalf("expected stale message, got %s", raw)
	}
}

func TestEventUnknownType(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ev4.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	token := issue(t, s)

	in := map[string]interface{}{"type": "nonexistent", "data": json.RawMessage(`{}`)}
	raw := apiCall(t, server.URL, token, "POST", "/v1/events", in, 200)
	if strings.Contains(string(raw), "text") {
		t.Fatalf("unknown event should return null, got %s", raw)
	}
}


