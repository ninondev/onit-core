package main

import (
	"errors"
	"testing"
	"time"
)

// The Java bridge caches the revision returned by read. A tick that changes
// state must return the new revision so the next ordinary edit can be written
// without a conflict. A stale client revision must still conflict.
func TestTickRevisionAdoptedForNextEdit(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	init := proactiveState(now)
	written, err := s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": init})))
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	cached := int(number(read["revision"]))
	if cached != int(number(written["revision"])) {
		t.Fatalf("read revision %d != write revision %d", cached, int(number(written["revision"])))
	}

	tick, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	if number(tick["made"]) != 1 {
		t.Fatalf("tick did not prepare a card: %s", encode(tick))
	}
	tickRevision := int(number(tick["revision"]))
	if tickRevision != cached+1 {
		t.Fatalf("tick revision %d, want %d", tickRevision, cached+1)
	}

	edited := tick["state"].(object)
	edited["name"] = "合成用户改动"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": tickRevision, "base": str(tick, "snapshot"), "state": edited}))); err != nil {
		t.Fatalf("edit after adopting tick revision failed: %v", err)
	}
	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "name") != "合成用户改动" {
		t.Fatal("edit after tick was not persisted")
	}

	// A client that kept the pre-tick revision must still be rejected, not
	// silently overwritten with its old snapshot.
	stale := tick["state"].(object)
	stale["name"] = "不应覆盖的旧快照"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": cached, "state": stale}))); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision should conflict, got %v", err)
	}
}

func TestTickWithoutChangeKeepsRevisionUsable(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	init := proactiveState(now)
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": init}))); err != nil {
		t.Fatal(err)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	revision := int(number(read["revision"]))

	// First tick creates a card; second tick is a no-change tick.
	if _, err = s.androidRequest([]byte(encode(object{"op": "tick"}))); err != nil {
		t.Fatal(err)
	}
	second, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	if second["changed"].(bool) {
		t.Fatalf("second tick should not change state: %s", encode(second))
	}
	noChangeRevision := int(number(second["revision"]))
	if noChangeRevision != revision+1 {
		t.Fatalf("no-change tick revision %d, want %d", noChangeRevision, revision+1)
	}
	state := second["state"].(object)
	state["name"] = "无变化 tick 后的编辑"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": noChangeRevision, "base": str(second, "snapshot"), "state": state}))); err != nil {
		t.Fatalf("edit after no-change tick failed: %v", err)
	}
}

func TestStaleSnapshotWithFreshRevisionConflicts(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	state := proactiveState(now)
	state["goals"] = []any{} // no eligible goal, so the tick makes no card
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	read1, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	r1 := int(number(read1["revision"]))
	s1 := str(read1, "snapshot")
	if s1 == "" {
		t.Fatal("read must return a snapshot token")
	}
	s1State := read1["state"].(object)

	// Another writer commits S2 from r1/S1.
	s2State := object{}
	for key, value := range s1State {
		s2State[key] = value
	}
	s2State["name"] = "另一写者的 S2"
	write2, err := s.androidRequest([]byte(encode(object{"op": "write", "revision": r1, "base": s1, "state": s2State})))
	if err != nil {
		t.Fatalf("second writer failed: %v", err)
	}
	r2 := int(number(write2["revision"]))
	s2 := str(write2, "snapshot")
	if r2 != r1+1 || s2 == "" || s2 == s1 {
		t.Fatalf("second writer did not advance the snapshot: %s", encode(write2))
	}

	// A no-change tick returns the server's r2/S2. The old frontend still
	// holds r1/S1, but adopts r2 from the tick.
	tick, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	if tick["changed"] != false || int(number(tick["revision"])) != r2 || str(tick, "snapshot") != s2 {
		t.Fatalf("no-change tick did not return the server snapshot: %s", encode(tick))
	}

	// The old edit is based on S1. Sending it with r2 alone must not pair the
	// old state with the new revision and silently overwrite S2.
	stale := object{}
	for key, value := range s1State {
		stale[key] = value
	}
	stale["name"] = "旧前端的编辑"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": r2, "base": s1, "state": stale}))); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale snapshot should conflict, got %v", err)
	}

	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "name") != "另一写者的 S2" {
		t.Fatalf("S2 was overwritten: %s", encode(after["state"]))
	}
}

func TestSamePayloadIsIdempotentAndOtherWriterStillConflicts(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	state := proactiveState(now)
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	revision := int(number(read["revision"]))
	edited := read["state"].(object)
	edited["name"] = "合成幂等编辑"
	payload := object{"op": "write", "revision": revision, "base": str(read, "snapshot"), "state": edited}
	first, err := s.androidRequest([]byte(encode(payload)))
	if err != nil {
		t.Fatal(err)
	}
	// The same payload sent again (a double tap or a retry after an
	// interrupted reply) must be accepted without writing twice.
	second, err := s.androidRequest([]byte(encode(payload)))
	if err != nil {
		t.Fatalf("same payload retry was rejected: %v", err)
	}
	if number(second["revision"]) != number(first["revision"]) {
		t.Fatalf("retry advanced revision: first=%v second=%v", first["revision"], second["revision"])
	}
	// A different payload with the old revision is a real concurrent writer
	// and must still conflict, not silently overwrite the saved edit.
	otherState := object{}
	for key, value := range edited {
		otherState[key] = value
	}
	otherState["name"] = "不应覆盖的并发写入"
	other := object{"op": "write", "revision": revision, "base": str(read, "snapshot"), "state": otherState}
	if _, err = s.androidRequest([]byte(encode(other))); !errors.Is(err, ErrConflict) {
		t.Fatalf("other writer should conflict, got %v", err)
	}
	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "name") != "合成幂等编辑" {
		t.Fatalf("saved edit was overwritten: %s", encode(after["state"]))
	}
}

func TestWriteWithoutSnapshotIsRejectedAfterFirstRevision(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	state := proactiveState(now)
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	revision := int(number(read["revision"]))
	edited := object{}
	for key, value := range read["state"].(object) {
		edited[key] = value
	}
	edited["name"] = "只带版本号的写入"
	// A revision alone must not be enough for a new write: the client has to
	// present the snapshot it read, or an old state could be paired with a
	// newer revision and silently replace it.
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": revision, "state": edited}))); !errors.Is(err, ErrConflict) {
		t.Fatalf("write without base should conflict, got %v", err)
	}
	// An exact retry of the current payload is still idempotent without a
	// base, so an interrupted reply can be retried safely.
	retry := object{"op": "write", "revision": revision, "state": read["state"]}
	if _, err = s.androidRequest([]byte(encode(retry))); err != nil {
		t.Fatalf("exact retry without base failed: %v", err)
	}
	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "name") == "只带版本号的写入" {
		t.Fatal("write without base was applied")
	}
}

func TestDelayedReadReplyCannotOverwriteNewerWrite(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	state := proactiveState(now)
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	// This is the reply a slow reader received before the other writer ran.
	delayed, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	delayedRevision := int(number(delayed["revision"]))
	delayedSnapshot := str(delayed, "snapshot")
	delayedState := delayed["state"].(object)

	// Another writer commits S2 while the first reply is still in flight.
	newer := object{}
	for key, value := range delayedState {
		newer[key] = value
	}
	newer["name"] = "新写者的 S2"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": delayedRevision, "base": delayedSnapshot, "state": newer}))); err != nil {
		t.Fatal(err)
	}

	// The delayed reader now submits an edit based on its old state and old
	// revision. It must conflict, not overwrite S2.
	stale := object{}
	for key, value := range delayedState {
		stale[key] = value
	}
	stale["name"] = "迟到的旧编辑"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": delayedRevision, "base": delayedSnapshot, "state": stale}))); !errors.Is(err, ErrConflict) {
		t.Fatalf("delayed read should conflict, got %v", err)
	}
	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "name") != "新写者的 S2" {
		t.Fatalf("newer write was overwritten: %s", encode(after["state"]))
	}
}

func TestArchiveIdentitySurvivesRealStore(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	state := proactiveState(now)
	state["archiveId"] = "archive-synthetic-1"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": 0, "state": state}))); err != nil {
		t.Fatal(err)
	}
	read, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(read["state"].(object), "archiveId") != "archive-synthetic-1" {
		t.Fatalf("archive identity was dropped by the store: %s", encode(read["state"]))
	}
	tick, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(tick["state"].(object), "archiveId") != "archive-synthetic-1" {
		t.Fatalf("tick changed the archive identity: %s", encode(tick["state"]))
	}
	// A tick is the same archive: an edit written after it must still carry
	// the identity the pending memory extraction captured.
	edited := tick["state"].(object)
	edited["name"] = "tick 后的编辑"
	if _, err = s.androidRequest([]byte(encode(object{"op": "write", "revision": tick["revision"], "base": tick["snapshot"], "state": edited}))); err != nil {
		t.Fatal(err)
	}
	after, err := s.androidRequest([]byte(encode(object{"op": "read"})))
	if err != nil {
		t.Fatal(err)
	}
	if str(after["state"].(object), "archiveId") != "archive-synthetic-1" {
		t.Fatalf("archive identity changed after an edit: %s", encode(after["state"]))
	}
}

func TestTickWithoutStateDoesNotInventRevision(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tick, err := s.androidRequest([]byte(encode(object{"op": "tick"})))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tick["revision"]; ok {
		t.Fatalf("empty tick must not report a revision: %s", encode(tick))
	}
}
