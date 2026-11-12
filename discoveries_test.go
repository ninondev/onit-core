package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// requireFrontend skips tests that replay browser modules from the frontend
// tree, which is not part of this repository.
func requireFrontend(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("../src/web/core.js"); err != nil {
		t.Skip("frontend modules not present in this repository")
	}
}

func rawGoalRef(src Source, quote string) object {
	start := 0
	for _, span := range firstReadingPassages(src) {
		if src.Text[span.start:span.end] == quote {
			start = span.start
			break
		}
	}
	a := len(utf16.Encode([]rune(src.Text[:start])))
	z := a + len(utf16.Encode([]rune(quote)))
	return object{"key": fmt.Sprintf("r:%s:%d:%d", src.ID, a, z), "version": 1, "quote": quote, "kind": src.Kind, "provenance": src.Kind + "-quote", "sourceBacked": true, "sourceId": src.ID, "start": a, "end": z}
}
func TestDiscoveryGoalHTTPPersistsAndUsesCurrentBasis(t *testing.T) {
	file := filepath.Join(t.TempDir(), "goals.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newService(s, Model{}))
	defer func() { server.Close(); s.Close() }()
	token := issue(t, s)
	user, _ := s.User(token)
	other := issue(t, s)
	src, err := s.Import(user, "file", "资料", "弦架旁有一块方石。\n背面刻着一行小字。", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(who, method, path string, in any, want int) []byte {
		return apiCall(t, server.URL, who, method, path, in, want)
	}
	input := object{"title": "方石背面的文字还需要核对。", "body": "顾问提出待核对的问题。", "kind": "question", "citations": []object{rawGoalRef(src, "弦架旁有一块方石。")}}
	d := unpack[object](t, request(token, "POST", "/v1/discoveries", input, 200))
	id := str(d, "id")
	refs := []GoalBasisRef{{Kind: "discovery", ID: id, Version: 1}}
	request(other, "POST", "/v1/proposals", object{"title": "他人的资料", "basisRefs": refs}, 409)
	p := unpack[Proposal](t, request(token, "POST", "/v1/proposals", object{"title": "核对方石", "basisRefs": refs}, 200))
	if len(unpack[[]Goal](t, request(token, "GET", "/v1/goals", nil, 200))) != 0 {
		t.Fatal("review created goal")
	}
	choice := object{"title": "核对方石", "step": "先向工作室询问", "unit": "次", "basisRefs": refs, "methods": []GoalMethod{{Origin: "custom", Text: "先向工作室询问"}}}
	g := unpack[Goal](t, request(token, "POST", "/v1/proposals/"+p.ID+"/accept", choice, 200))
	if len(g.CurrentBasis) != 1 || g.CurrentBasis[0].Mode != "proposed" {
		t.Fatal(g)
	}
	mem, _ := s.Portrait(user)
	if len(mem) != 0 {
		t.Fatal("goal vote fabricated confirmed memory", mem)
	}
	corrected := unpack[object](t, request(token, "POST", "/v1/discoveries/"+id+"/correct", object{"version": 1, "text": "方石由工作室保管。"}, 200))
	bound := str(corrected, "correctionId")
	request(token, "PATCH", "/v1/memories/"+bound, object{"text": "方石由访客暂存。", "version": 1}, 200)
	if err = s.RemoveSource(user, src.ID, nil); err != nil {
		t.Fatal(err)
	}
	server.Close()
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(newService(s, Model{}))
	list := unpack[[]Goal](t, request(token, "GET", "/v1/goals", nil, 200))
	current := list[0]
	if current.ID != g.ID || current.Step != g.Step || encode(current.ApprovedMethods) != encode(g.ApprovedMethods) || current.ApprovedBasis[0].Version != 1 || current.CurrentBasis[0].Text != "方石由访客暂存。" || current.CurrentBasis[0].CurrentVersion != 3 || current.CurrentBasis[0].Mode != "corrected" {
		t.Fatalf("cold goal %+v", current)
	}
	if err = s.Forget(user, bound); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Goals(user)
	if list[0].CurrentBasis[0].Status != "stale" || list[0].Step != g.Step {
		t.Fatal(list)
	}
}
func TestDiscoveryGoalRejectsStaleAcceptAndWithdrawsOnlyBasis(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "goals.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	src, _ := s.Import(user, "file", "记录", "弦架旁有一块方石。", nil)
	d, err := s.CreateDiscovery(user, "需要核对方石。", "", "question", []object{rawGoalRef(src, src.Text)})
	if err != nil {
		t.Fatal(err)
	}
	refs := []GoalBasisRef{{Kind: "discovery", ID: str(d, "id"), Version: 1}}
	p, err := s.ProposeWithRefs(user, "核对记录", refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CorrectDiscovery(user, str(d, "id"), "方石属于工作室。", 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptProposalWithRefs(user, p.ID, p.Title, "向工作室询问", "次", refs, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale accepted", err)
	}
	if goals, _ := s.Goals(user); len(goals) != 0 {
		t.Fatal(goals)
	}
	if ps, _ := s.Proposals(user); len(ps) != 1 {
		t.Fatal("stale attempt consumed proposal")
	}
	d2, _ := s.CreateDiscovery(user, "核对方石原记录。", "", "finding", []object{rawGoalRef(src, src.Text)})
	ref2 := []GoalBasisRef{{Kind: "discovery", ID: str(d2, "id"), Version: 1}}
	p2, _ := s.ProposeWithRefs(user, "保管记录", ref2)
	g, err := s.AcceptProposalWithRefs(user, p2.ID, p2.Title, "先保留记录", "次", ref2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveSource(user, src.ID, nil); err != nil {
		t.Fatal(err)
	}
	goals, err := s.Goals(user)
	if err != nil || goals[0].CurrentBasis[0].Status != "stale" || goals[0].Step != g.Step {
		t.Fatal(goals, err)
	}
}
func TestDiscoveryCitationMatchesBrowserTrimmedComponent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trim.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	user, _ := storeUser(t, s)
	src, err := s.Import(user, "file", "记录", "弦架旁有一块方石。 不过要提前两天说。", nil)
	if err != nil {
		t.Fatal(err)
	}
	var lead, qualifier firstReadingPassage
	for _, span := range firstReadingPassages(src) {
		if len(span.components) == 2 {
			lead, qualifier = span.components[0], span.components[1]
		}
	}
	if qualifier.end == 0 || lead.end == 0 {
		t.Fatal("no merged qualifier span")
	}
	if raw := src.Text[qualifier.start:qualifier.end]; raw == firstReadingTrim(raw) {
		t.Fatalf("component has no original whitespace to diverge on: %q", raw)
	}
	// Withdrawing the first sentence suppresses the merged parent, so the
	// browser offers the remaining component trimmed to its quote offsets.
	if err = s.write(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO dismissed VALUES(?,?,?,?)", user, src.ID, lead.start, lead.end)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	quote := firstReadingTrim(src.Text[qualifier.start:qualifier.end])
	trimStart := qualifier.start + strings.Index(src.Text[qualifier.start:qualifier.end], quote)
	a := len(utf16.Encode([]rune(src.Text[:trimStart])))
	z := a + len(utf16.Encode([]rune(quote)))
	ref := object{"key": fmt.Sprintf("r:%s:%d:%d", src.ID, a, z), "version": 1, "quote": quote,
		"kind": src.Kind, "provenance": src.Kind + "-quote", "sourceBacked": true, "sourceId": src.ID, "start": a, "end": z}
	if _, err = s.CreateDiscovery(user, "看房前要提前两天说。", "", "finding", []object{ref}); err != nil {
		t.Fatalf("browser-style trimmed component citation rejected: %v", err)
	}
	// Raw dossier input cannot arrive as corrected without its fact reference.
	fabricated := object{"id": "fabricated", "version": 1, "status": "corrected",
		"title": "伪造为本人已更正。", "kind": "finding", "citations": []object{ref}}
	if err = s.write(func(tx *sql.Tx) error {
		return saveDiscoveryTx(tx, user, fabricated)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fabricated corrected label accepted: %v", err)
	}
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Discoveries(user)
	if err != nil || len(rows) != 1 || str(rows[0], "status") != "proposed" {
		t.Fatalf("cold refresh lost trimmed component discovery: %v %v", rows, err)
	}
}
func TestDiscoveryGoalRestoreReplaysArchivedStaleBasis(t *testing.T) {
	requireFrontend(t)
	script := `const S=require('../src/web/core.js'),C=require('../src/web/first-reading-context.js'),F=require('../src/web/first-reading.js'),D=require('../src/web/discovery-basis.js'),R=require('../src/web/reveal-reading.js');const s=S.empty();S.importSource(s,'原件','弦架旁有一块方石。','document','file');F.prepare(s);const refs=C.refs(s),req=F.begin(s,refs,'controlled');F.finish(s,req.id,'controlled',JSON.stringify({title:'方石的记录需要核对。',why:'顾问提出的问题。',kind:'question',citations:refs.map(({key,version,quote})=>({key,version,quote}))}),req.attemptId);const id=s.reveal.firstReading.discoveryId,p=S.propose(s,'核对记录',D.reference(s,id),{manual:true}),review=R.goalProposalReview(s,p);S.accept(s,p.id,p.title,'先向工作室询问','次',{basis:review.facts.map(f=>f.token),methods:[]},R.goalProposalReview);console.log(JSON.stringify(s));`
	raw, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state})
	var user string
	if err = s.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&user); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query("SELECT id FROM sources WHERE user_id=?", user)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	// The user withdraws the only source after accepting the goal; the app
	// exports a backup from exactly this state.
	for _, id := range ids {
		if err = s.RemoveSource(user, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	backup := bridgeRequest(t, s, object{"op": "read"})["state"].(object)

	// Restoring that backup on a fresh device must replay the archive: the
	// goal survives with its basis honestly stale, step and methods intact.
	restored, err := OpenStore(filepath.Join(t.TempDir(), "restore.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	bridgeRequest(t, restored, object{"op": "write", "revision": 0, "state": backup})
	var other string
	if err = restored.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&other); err != nil {
		t.Fatal(err)
	}
	goals, err := restored.Goals(other)
	if err != nil || len(goals) != 1 {
		t.Fatalf("archived goal lost on restore: %v %v", goals, err)
	}
	if goals[0].Step != "先向工作室询问" || goals[0].CurrentBasis[0].Status != "stale" || goals[0].ApprovedBasis[0].Version != 1 {
		t.Fatalf("restore did not replay the archive: %+v", goals[0])
	}
}
func TestDiscoveryGoalBrowserContractAndOlderClient(t *testing.T) {
	requireFrontend(t)
	script := `const S=require('../src/web/core.js'),C=require('../src/web/first-reading-context.js'),F=require('../src/web/first-reading.js'),D=require('../src/web/discovery-basis.js'),R=require('../src/web/reveal-reading.js');const s=S.empty();S.importSource(s,'原件','弦架旁有一块方石。\n背面刻着一行小字。','document','file');F.prepare(s);const refs=C.refs(s),req=F.begin(s,refs,'controlled');F.finish(s,req.id,'controlled',JSON.stringify({title:'方石的记录需要核对。',why:'顾问提出的问题。',kind:'question',citations:refs.map(({key,version,quote})=>({key,version,quote}))}),req.attemptId);const id=s.reveal.firstReading.discoveryId,p=S.propose(s,'核对记录',D.reference(s,id),{manual:true}),review=R.goalProposalReview(s,p);S.accept(s,p.id,p.title,'先向工作室询问','次',{basis:review.facts.map(f=>f.token),methods:[]},R.goalProposalReview);console.log(JSON.stringify(s));`
	raw, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, user, file := openFirstReading(t, state)
	defer func() { s.Close() }()
	goals, err := s.Goals(user)
	if err != nil || len(goals) != 1 || goals[0].CurrentBasis[0].Mode != "proposed" {
		t.Fatal(goals, err)
	}
	snapshot := bridgeRequest(t, s, object{"op": "read"})
	read := snapshot["state"].(object)
	delete(read, "discoveries")
	read["name"] = "旧客户端改名字"
	bridgeRequest(t, s, object{"op": "write", "revision": snapshot["revision"], "base": snapshot["snapshot"], "state": read})
	rows, err := s.Discoveries(user)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	id := str(rows[0], "id")
	if _, err = s.CorrectDiscovery(user, id, "方石由工作室保管。", 1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	saved := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	check := `let s=JSON.parse(require('fs').readFileSync(0,'utf8'));const D=require('../src/web/discovery-basis.js'),R=require('../src/web/reveal-reading.js'),F=require('../src/web/first-reading.js');if(D.resolve(s,s.goals[0].approvedBasis[0].id).title!=='方石由工作室保管。'||F.opening(s).title!=='方石由工作室保管。')throw Error('current JS view lost');`
	cmd := exec.Command("node", "-e", check)
	cmd.Stdin = strings.NewReader(encode(saved))
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
}
