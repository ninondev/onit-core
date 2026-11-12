package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestGoalReviewHTTPAccountMatchesPreparation(t *testing.T) {
	s := newPreparationStore(t)
	user, token := storeUser(t, s)
	src, fact, goal, _ := seedFollowReview(t, s, user)
	enableFollowReview(t, s, user, goal, src, fact, "http-account-1", 0)

	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()

	prepRaw := apiCall(t, server.URL, token, "GET", "/v1/preparation", nil, 200)
	var prep object
	if err := json.Unmarshal(prepRaw, &prep); err != nil {
		t.Fatal(err)
	}
	listRaw := apiCall(t, server.URL, token, "GET", "/v1/goal-review", nil, 200)
	var list object
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatal(err)
	}
	oneRaw := apiCall(t, server.URL, token, "GET", "/v1/goal-review/"+goal, nil, 200)
	var one object
	if err := json.Unmarshal(oneRaw, &one); err != nil {
		t.Fatal(err)
	}

	want := preparationAccount(user)
	if str(prep, "account") != want {
		t.Fatalf("preparation account=%q want %q", str(prep, "account"), want)
	}
	if str(list, "account") != want {
		t.Fatalf("list account=%q want %q", str(list, "account"), want)
	}
	if str(one, "account") != want {
		t.Fatalf("item account=%q want %q", str(one, "account"), want)
	}
	if str(one, "account") != str(prep, "account") {
		t.Fatalf("goal-review account %q != preparation %q", str(one, "account"), str(prep, "account"))
	}
	follows, _ := list["follows"].([]any)
	if len(follows) == 0 {
		t.Fatal("missing follows")
	}
	first, _ := follows[0].(map[string]any)
	if str(object(first), "account") != want {
		t.Fatalf("follow account=%q want %q", str(object(first), "account"), want)
	}
}

func TestGoalReviewHTTPUnauthorizedHasNoAccount(t *testing.T) {
	s := newPreparationStore(t)
	server := httptest.NewServer(newService(s, Model{}))
	defer server.Close()
	raw := apiCall(t, server.URL, "", "GET", "/v1/goal-review", nil, 401)
	var body object
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if str(body, "error") != "authentication_required" {
		t.Fatalf("error=%q", str(body, "error"))
	}
	if _, ok := body["account"]; ok {
		t.Fatalf("unauthenticated body leaked account: %v", body)
	}
}
