package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

const mobileFixture = `{
  "version":2,"name":"河川","futureField":{"n":9007199254740993},"profile":null,"model":null,
  "sources":[{"id":"s","kind":"self","origin":"chat","title":"手记","text":"😀展厅七点关门。","at":"2026-11-01T10:00:00.000Z","oldSourceField":true}],
  "facts":[{"id":"f","sourceId":"s","source":"手记","text":"展厅七点关门。","quote":"展厅七点关门。","start":2,"end":9,
    "category":"state","attribution":"self","confirmed":false,"version":2,"at":"2026-11-01T10:01:00.000Z",
    "evidence":[{"sourceId":"s","start":2,"end":9,"quote":"展厅七点关门。","oldSpan":1}],"history":[{"text":"六点关门","at":"昨日","oldHistory":1}],"alternatives":[]}],
  "dismissed":[{"sourceId":"s","start":0,"end":2,"reason":"manual"}],
  "conversation":[{"id":"u","role":"user","text":"几点关门？","at":"同一时刻"},
    {"id":"reply","role":"assistant","text":"七点","factIds":["f"],"at":"同一时刻"},{"id":"invite","kind":"portrait-invite","role":"assistant"}],
  "reveal":{"opened":true,"firstId":"f","lastSource":"s","opening":{"title":"旧解读","citations":[{"id":"f","quote":"展厅七点关门。"}]}},
  "proposals":[{"id":"p","title":"","titleFromFact":true,"basis":"f","factIds":["f"],"at":"昨天"}],
  "goals":[{"id":"g","title":"按时到","step":"提前五分钟","unit":"次","at":"2026-11-01T11:00:00.000Z","proposalId":"accepted",
     "approvedBasis":[{"id":"f","version":1,"archived":true},{"id":"withdrawn","version":3}],"approvedMethods":[{"origin":"fact","factId":"f","version":1,"text":"记下关门时间"},{"origin":"custom","text":"提早出门"}],
     "entries":[{"id":"e","amount":1.5,"note":"走路","at":"2026-11-01T12:00:00.000Z","ext":1}]}],
  "reports":[{"id":"r","at":"2026-11-03T12:00:00.000Z","from":"2026-10-28T00:00:00.000Z","rows":[{"goalId":"g","title":"按时到","step":"提前五分钟","unit":"次","count":1,"amount":1.5,"notes":["走路"],"extraRow":1}]}]
}`

func bridgeRequest(t *testing.T, s *Store, op object) object {
	t.Helper()
	out, err := s.androidRequest([]byte(encode(op)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAndroidDossierRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	read := object{"op": "read"}
	if r := bridgeRequest(t, s, read); r["revision"] != 0 || encode(r["state"]) != "null" {
		t.Fatal(r)
	}
	original, err := parseObject([]byte(mobileFixture))
	if err != nil {
		t.Fatal(err)
	}
	write := object{"op": "write", "revision": 0, "state": original}
	if r := bridgeRequest(t, s, write); r["revision"] != 1 {
		t.Fatal(r)
	}
	if r := bridgeRequest(t, s, write); r["revision"] != 1 {
		t.Fatal("retry", r)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := bridgeRequest(t, s, read)
	state := r["state"].(object)
	if encode(original) != encode(state) {
		t.Fatalf("round trip:\n%s\n%s", encode(original), encode(state))
	}
	var user string
	if err = s.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&user); err != nil {
		t.Fatal(err)
	}
	other, _ := storeUser(t, s)
	if found, err := s.Sources(other); err != nil || len(found) != 0 {
		t.Fatal(found, err)
	}
	if found, err := s.Sources(user); err != nil || len(found) != 1 || found[0].ID != "s" {
		t.Fatal(found, err)
	}
	mem, err := s.Portrait(user)
	if err != nil || len(mem) != 1 || mem[0].Evidence[0].Start != 4 || mem[0].Evidence[0].End != 25 {
		t.Fatal(mem, err)
	}
	goals, err := s.Goals(user)
	if err != nil || len(goals) != 1 || len(goals[0].ApprovedBasis) != 2 || goals[0].ApprovedMethods[1].Text != "提早出门" || goals[0].Basis.CurrentVersion != 2 || goals[0].Entries[0].ID != "e" {
		t.Fatal(goals, err)
	}
	if _, err = s.Correct(user, "f", "展厅八点关门。", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.androidRequest([]byte(encode(write))); !errors.Is(err, ErrConflict) {
		t.Fatal("stale retry", err)
	}
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 1, "state": original}))); !errors.Is(err, ErrConflict) {
		t.Fatal("stale replace", err)
	}
	after := bridgeRequest(t, s, read)
	if after["revision"] != 2 {
		t.Fatal(after)
	}
	state = after["state"].(object)
	facts, _ := objects(state["facts"])
	chat, _ := objects(state["conversation"])
	history, _ := objects(facts[0]["history"])
	if facts[0]["text"] != "展厅八点关门。" || len(history) != 2 || history[0]["at"] != "昨日" || history[1]["at"] != "2026-11-01T10:01:00.000Z" || chat[1]["basisCorrected"] != true || state["reveal"].(object)["opening"] != nil {
		t.Fatal(state)
	}
	n := float64(3)
	if err = s.ChangeGoalEntry(user, "g", "e", &n, nil); err != nil {
		t.Fatal(err)
	}
	after = bridgeRequest(t, s, read)
	reports, _ := objects(after["state"].(object)["reports"])
	rows, _ := objects(reports[0]["rows"])
	if encode(rows[0]["amount"]) != "3" || rows[0]["extraRow"] == nil {
		t.Fatal(rows)
	}
	if err = s.RemoveSource(user, "s", nil); err != nil {
		t.Fatal(err)
	}
	after = bridgeRequest(t, s, read)
	state = after["state"].(object)
	for _, k := range []string{"sources", "facts", "proposals", "dismissed"} {
		rows, _ := objects(state[k])
		if len(rows) != 0 {
			t.Fatal(k, rows)
		}
	}
	chat, _ = objects(state["conversation"])
	if len(chat) != 2 || chat[0]["id"] != "u" || chat[1]["id"] != "invite" {
		t.Fatal(chat)
	}
	basis := state["reveal"].(object)
	if basis["firstId"] != nil || basis["lastSource"] != nil {
		t.Fatal(basis)
	}
}

func TestAndroidDossierTransactions(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, _ := storeUser(t, s)
	src, err := s.Import(other, "file", "私有", "第三方资料。", nil)
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := parseObject([]byte(mobileFixture))
	bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": orig})
	snap := bridgeRequest(t, s, object{"op": "read"})
	for _, bad := range []func(object){
		func(d object) { sources, _ := objects(d["sources"]); sources[0]["id"] = src.ID },
		func(d object) {
			facts, _ := objects(d["facts"])
			ev, _ := objects(facts[0]["evidence"])
			ev[0]["start"] = 1
		},
		func(d object) {
			facts, _ := objects(d["facts"])
			ev, _ := objects(facts[0]["evidence"])
			ev[0]["quote"] = "另一句"
		},
		func(d object) {
			goals, _ := objects(d["goals"])
			entries, _ := objects(goals[0]["entries"])
			entries[0]["amount"] = -1
		},
	} {
		d, _ := parseObject([]byte(mobileFixture))
		d["name"] = "不应落入库"
		bad(d)
		if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": snap["revision"], "base": snap["snapshot"], "state": d}))); err == nil {
			t.Fatal("invalid write accepted")
		}
		if r := bridgeRequest(t, s, object{"op": "read"}); r["revision"] != 1 || encode(r["state"]) != encode(orig) {
			t.Fatal("partial write", r)
		}
	}
	if got, e := s.Source(other, src.ID); e != nil || got.Text != "第三方资料。" {
		t.Fatal(got, e)
	}
	orig["name"] = "更名"
	a, b := make(chan error, 1), make(chan error, 1)
	raw := encode(object{"op": "write", "revision": snap["revision"], "base": snap["snapshot"], "state": orig})
	go func() { _, e := s.androidRequest([]byte(raw)); a <- e }()
	orig["name"] = "另一个名字"
	raw2 := encode(object{"op": "write", "revision": snap["revision"], "base": snap["snapshot"], "state": orig})
	go func() { _, e := s.androidRequest([]byte(raw2)); b <- e }()
	x, y := <-a, <-b
	if !((x == nil && errors.Is(y, ErrConflict)) || (y == nil && errors.Is(x, ErrConflict))) {
		t.Fatal(x, y)
	}
}

func TestAndroidStdio(t *testing.T) {
	s, e := OpenStore(":memory:")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	in := bytes.NewBufferString("broken\n{\"op\":\"analyze\",\"kind\":\"self\",\"text\":\"?？\"}\n{\"op\":\"read\"}\n")
	var out bytes.Buffer
	if e = runAndroidBridge(s, in, &out); e != nil {
		t.Fatal(e)
	}
	dec := json.NewDecoder(&out)
	empty := sha256.Sum256([]byte("null"))
	// Offline analyze is gone: the op must fail loudly instead of pretending.
	for _, want := range []string{`{"error":"invalid_input","ok":false}`, `{"error":"invalid_input","ok":false}`, fmt.Sprintf(`{"ok":true,"revision":0,"snapshot":"%x","state":null}`, empty)} {
		var got object
		if e = dec.Decode(&got); e != nil || encode(got) != want {
			t.Fatal(got, e)
		}
	}
}
