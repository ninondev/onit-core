package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func firstReadingFixture(t *testing.T, ref object, inResultOnly bool) object {
	t.Helper()
	state, err := parseObject([]byte(mobileFixture))
	if err != nil {
		t.Fatal(err)
	}
	state["sources"] = append(state["sources"].([]any),
		object{"id": "copy", "kind": "self", "origin": "note", "title": "另一份手记", "text": "前台七点熄灯。"},
		object{"id": "same-quote", "kind": "self", "origin": "note", "title": "改名抄本", "text": "😀展厅七点关门。"},
		object{"id": "unrelated", "kind": "self", "origin": "note", "title": "画笔", "text": "我习惯星期六整理画笔。"})
	facts, _ := objects(state["facts"])
	facts[0]["evidence"] = append(facts[0]["evidence"].([]any), object{"sourceId": "copy", "start": 0, "end": 7, "quote": "前台七点熄灯。"})
	selection := []any{ref}
	if inResultOnly {
		selection = []any{}
	}
	state["reveal"].(object)["firstReading"] = object{
		"id": "reading-1", "status": "complete", "destination": "lan|owner-selected|loopback", "address": "https://example.test/v1/chat/completions", "model": "owner-selected",
		"selection": selection,
		"result":    object{"title": "顾问提出的一句判断", "body": "依据展厅七点关门。", "citations": []any{ref}},
	}
	state["conversation"] = append(state["conversation"].([]any),
		object{"id": "discovery", "role": "assistant", "kind": "first-discovery", "firstReadingId": "reading-1", "text": "顾问提出的一句判断"},
		object{"id": "other-reading", "role": "assistant", "kind": "first-discovery", "firstReadingId": "another-reading", "text": "另一条原聊天"})
	return state
}

func openFirstReading(t *testing.T, state object) (*Store, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reading.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state})
	var user string
	if err = s.db.QueryRow("SELECT user_id FROM android_dossier").Scan(&user); err != nil {
		t.Fatal(err)
	}
	return s, user, path
}

func assertFirstReadingStale(t *testing.T, s *Store, withdrawn bool) {
	t.Helper()
	state := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	task := state["reveal"].(object)["firstReading"].(object)
	if task["id"] != "reading-1" || task["status"] != "stale" || task["selection"] != nil || task["result"] != nil ||
		task["destination"] != "lan|owner-selected|loopback" || task["address"] != "https://example.test/v1/chat/completions" || task["model"] != "owner-selected" || str(task, "error") == "" {
		t.Fatalf("pending/current content was not removed: %s", encode(task))
	}
	if strings.Contains(encode(task), "展厅七点关门") {
		t.Fatal("old quote survived in first-reading metadata")
	}
	chat, _ := objects(state["conversation"])
	found := map[string]object{}
	for _, message := range chat {
		found[str(message, "id")] = message
	}
	if withdrawn {
		if found["discovery"] != nil {
			t.Fatal("withdrawn discovery survived", found["discovery"])
		}
	} else if found["discovery"]["basisCorrected"] != true {
		t.Fatal("old discovery still current", found["discovery"])
	}
	if found["u"] == nil || found["invite"] == nil || found["other-reading"] == nil {
		t.Fatal("unrelated conversation or invitation changed", chat)
	}
	goals, _ := objects(state["goals"])
	if len(goals) != 1 || goals[0]["step"] != "提前五分钟" {
		t.Fatal("user action changed", goals)
	}
	methods, _ := objects(goals[0]["approvedMethods"])
	if len(methods) != 2 || methods[1]["text"] != "提早出门" {
		t.Fatal("accepted methods changed", methods)
	}
}

func TestFirstReadingCorrectionInvalidatesCurrentReferences(t *testing.T) {
	for _, test := range []struct {
		name       string
		ref        object
		resultOnly bool
	}{
		{"fact", object{"key": "f:f", "id": "f", "version": 2, "sourceId": "s", "quote": "展厅七点关门。"}, false},
		{"unextracted-source", object{"key": "r:s:2:9", "sourceId": "s", "quote": "展厅七点关门。"}, false},
		{"secondary-evidence", object{"key": "r:copy:0:7", "sourceId": "copy", "quote": "前台七点熄灯。"}, false},
		{"renamed-copy", object{"key": "r:same-quote:2:9", "sourceId": "same-quote", "quote": "展厅七点关门。"}, false},
		{"result-citation-only", object{"key": "f:f", "id": "f", "version": 2, "quote": "展厅七点关门。"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			initial := firstReadingFixture(t, test.ref, test.resultOnly)
			s, user, path := openFirstReading(t, initial)
			if _, err := s.Correct(user, "f", "展厅八点关门。", 2); err != nil {
				t.Fatal(err)
			}
			if _, err := s.androidRequest([]byte(encode(object{"op": "write", "revision": 1, "state": initial}))); !errors.Is(err, ErrConflict) {
				t.Fatal("stale mobile reply replaced current state", err)
			}
			s.Close()
			s, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			assertFirstReadingStale(t, s, false)
		})
	}
}

func TestFirstReadingConfirmationAdvancesProvenanceVersion(t *testing.T) {
	s, user, _ := openFirstReading(t, firstReadingFixture(t, object{"key": "f:f", "id": "f", "version": 2, "sourceId": "s", "quote": "展厅七点关门。"}, false))
	defer s.Close()
	confirmed, err := s.Correct(user, "f", "展厅七点关门。", 2)
	if err != nil || !confirmed.Confirmed || confirmed.Version != 3 {
		t.Fatal(confirmed, err)
	}
	assertFirstReadingStale(t, s, false)
	before := bridgeRequest(t, s, object{"op": "read"})
	if again, err := s.Correct(user, "f", confirmed.Text, confirmed.Version); err != nil || again.Version != confirmed.Version {
		t.Fatal("confirmed same-text no-op changed", again, err)
	}
	if after := bridgeRequest(t, s, object{"op": "read"}); encode(after) != encode(before) {
		t.Fatal("same confirmed words changed the stored task")
	}
}

func TestFirstReadingWithdrawalClearsRawAndFactCaches(t *testing.T) {
	for _, operation := range []string{"forget", "source"} {
		t.Run(operation, func(t *testing.T) {
			s, user, _ := openFirstReading(t, firstReadingFixture(t, object{"key": "r:s:2:9", "sourceId": "s", "quote": "展厅七点关门。"}, false))
			defer s.Close()
			var err error
			if operation == "forget" {
				err = s.Forget(user, "f")
			} else {
				// Another source still supports f. The raw source citation must
				// expire even when the memory survives source removal.
				err = s.RemoveSource(user, "s", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertFirstReadingStale(t, s, true)
			if operation == "source" {
				memories, err := s.Portrait(user)
				if err != nil || len(memories) != 1 || memories[0].SourceID != "copy" {
					t.Fatal("surviving independent evidence lost", memories, err)
				}
			}
		})
	}
}

func TestFirstReadingUnrelatedMaterialAndPersonStayCurrent(t *testing.T) {
	initial := firstReadingFixture(t, object{"key": "r:unrelated:0:12", "sourceId": "unrelated", "quote": "我习惯星期六整理画笔。"}, false)
	// The result in this case only refers to the selected unrelated material.
	initial["reveal"].(object)["firstReading"].(object)["result"].(object)["body"] = "依据画笔手记。"
	s, user, _ := openFirstReading(t, initial)
	defer s.Close()
	before := encode(initial["reveal"].(object)["firstReading"])
	other, _ := storeUser(t, s)
	if _, err := s.Import(other, "self", "另一人", "我每周练习吉他。", quotesToFindings([]string{"我每周练习吉他。"})); err != nil {
		t.Fatal(err)
	}
	memories, err := s.Portrait(other)
	if err != nil || len(memories) != 1 {
		t.Fatal(memories, err)
	}
	if _, err = s.Correct(other, memories[0].ID, "我每月练习口琴。", memories[0].Version); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Correct(user, "f", "展厅八点关门。", 2); err != nil {
		t.Fatal(err)
	}
	after := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	if encode(after["reveal"].(object)["firstReading"]) != before {
		t.Fatal("unrelated selected material was invalidated")
	}
	chat, _ := objects(after["conversation"])
	for _, message := range chat {
		if str(message, "id") == "discovery" && message["basisCorrected"] != nil {
			t.Fatal("unrelated discovery was changed", message)
		}
	}
}

func TestFirstReadingRawOnlySourceRemoval(t *testing.T) {
	initial := firstReadingFixture(t, object{"key": "r:unrelated:0:12", "sourceId": "unrelated", "quote": "我习惯星期六整理画笔。"}, false)
	s, user, _ := openFirstReading(t, initial)
	defer s.Close()
	if err := s.RemoveSource(user, "unrelated", nil); err != nil {
		t.Fatal(err)
	}
	assertFirstReadingStale(t, s, true)
	memories, err := s.Portrait(user)
	if err != nil || len(memories) != 1 || memories[0].SourceID != "s" {
		t.Fatal("unrelated facts changed", memories, err)
	}
}

func TestFirstReadingSecondarySourceRemovalAndRollback(t *testing.T) {
	initial := firstReadingFixture(t, object{"key": "f:f", "id": "f", "version": 2, "sourceId": "s", "quote": "展厅七点关门。"}, false)
	s, user, _ := openFirstReading(t, initial)
	defer s.Close()
	before := bridgeRequest(t, s, object{"op": "read"})
	if err := s.RemoveSource(user, "s", nil, ReadingRevision{ID: "f", Version: 999}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if after := bridgeRequest(t, s, object{"op": "read"}); encode(before) != encode(after) {
		t.Fatal("failed source removal left partial metadata", after)
	}
	if err := s.RemoveSource(user, "copy", nil); err != nil {
		t.Fatal(err)
	}
	assertFirstReadingStale(t, s, true)
	memories, err := s.Portrait(user)
	if err != nil || len(memories) != 1 || memories[0].SourceID != "s" || memories[0].Version != 3 {
		t.Fatal(memories, err)
	}
}

func TestFirstReadingSameSentenceInIndependentDocument(t *testing.T) {
	initial := firstReadingFixture(t, object{"key": "r:unrelated:0:12", "sourceId": "unrelated", "quote": "展厅七点关门。"}, false)
	sources, _ := objects(initial["sources"])
	for _, source := range sources {
		if source["id"] == "unrelated" {
			source["text"] = "另一位记录者今天另行确认。展厅七点关门。"
		}
	}
	s, user, _ := openFirstReading(t, initial)
	defer s.Close()
	before := encode(initial["reveal"].(object)["firstReading"])
	if _, err := s.Correct(user, "f", "展厅八点关门。", 2); err != nil {
		t.Fatal(err)
	}
	after := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	if encode(after["reveal"].(object)["firstReading"]) != before {
		t.Fatal("independent corroboration was globally suppressed")
	}
}

func TestReadingReferencesWithoutFirstReadingTask(t *testing.T) {
	for _, operation := range []string{"correct", "confirm", "forget", "source"} {
		t.Run(operation, func(t *testing.T) {
			ref := object{"key": "r:s:2:9", "sourceId": "s", "quote": "展厅七点关门。"}
			initial := firstReadingFixture(t, ref, false)
			delete(initial["reveal"].(object), "firstReading")
			initial["conversation"] = append(initial["conversation"].([]any),
				object{"id": "raw-reply", "role": "assistant", "text": "展厅七点关门。", "readingReferences": []any{ref}},
				object{"id": "raw-user", "role": "user", "text": "我自己的问题保持原样。", "readingReferences": []any{ref}},
				object{"id": "unrelated-reply", "role": "assistant", "text": "画笔的事情。", "readingReferences": []any{object{"key": "r:unrelated:0:12", "sourceId": "unrelated", "quote": "我习惯星期六整理画笔。"}}})
			s, user, _ := openFirstReading(t, initial)
			defer s.Close()
			var err error
			switch operation {
			case "correct":
				_, err = s.Correct(user, "f", "展厅八点关门。", 2)
			case "confirm":
				_, err = s.Correct(user, "f", "展厅七点关门。", 2)
			case "forget":
				err = s.Forget(user, "f")
			case "source":
				err = s.RemoveSource(user, "s", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			state := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
			chat, _ := objects(state["conversation"])
			found := map[string]object{}
			for _, message := range chat {
				found[str(message, "id")] = message
			}
			if operation == "forget" || operation == "source" {
				if found["raw-reply"] != nil {
					t.Fatal("withdrawn reply survived", found["raw-reply"])
				}
			} else if found["raw-reply"]["basisCorrected"] != true || found["raw-reply"]["readingReferences"] != nil {
				t.Fatal("old raw quote remained current", found["raw-reply"])
			}
			if found["raw-user"] == nil || found["unrelated-reply"] == nil || found["unrelated-reply"]["basisCorrected"] != nil {
				t.Fatal("unrelated or original user messages changed", found)
			}
		})
	}
}

// The fixtures contain actual output from the browser's rememberSuppressed,
// including UTF-16 ranges. Exercise its Go counterpart through SQLite source
// removal and the normal Android dossier readback; no JavaScript runtime needed.
func TestFirstReadingSuppressionMatchesBrowserContract(t *testing.T) {
	data, err := os.ReadFile("testdata/first_reading_suppressions.json")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := parseObject(append(append([]byte(`{"cases":`), data...), '}'))
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := objects(wrapper["cases"])
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(str(fixture, "name"), func(t *testing.T) {
			state := fixture["state"].(object)
			s, user, path := openFirstReading(t, state)
			if err := s.RemoveSource(user, str(fixture, "sourceId"), nil); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got := bridgeRequest(t, s, object{"op": "read"})["state"].(object)["readingSuppressions"]
			if encode(got) != encode(fixture["expected"]) {
				t.Fatalf("browser/Go suppression mismatch\nGo: %s\nJS: %s", encode(got), encode(fixture["expected"]))
			}
		})
	}
}

func discoveryDependencyFixture(t *testing.T) object {
	t.Helper()
	ref := object{"key": "r:s:2:9", "version": 1, "sourceId": "s", "start": 2, "end": 9, "quote": "展厅七点关门。", "provenance": "personal-source"}
	state := firstReadingFixture(t, ref, false)
	task := state["reveal"].(object)["firstReading"].(object)
	task["attemptId"] = "attempt-1"
	task["revision"] = 1
	dependency := object{"id": "reading-1", "attemptId": "attempt-1", "revision": 1, "mode": "proposed", "citations": []any{ref}}
	state["conversation"] = append(state["conversation"].([]any),
		object{"id": "dependent-reply", "role": "assistant", "text": "沿着刚才的发现，再回答你的问题。", "discoveryReferences": []any{dependency}},
		object{"id": "dependent-user", "role": "user", "text": "刚才的发现意味着什么？", "discoveryReferences": []any{dependency}},
		object{"id": "independent-reply", "role": "assistant", "text": "画笔仍放在收纳盒。"})
	return state
}

func discoveryMessages(t *testing.T, state object) map[string]object {
	t.Helper()
	messages, err := objects(state["conversation"])
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]object{}
	for _, message := range messages {
		found[str(message, "id")] = message
	}
	return found
}

func assertDiscoveryDependents(t *testing.T, state object, removed bool) {
	t.Helper()
	found := discoveryMessages(t, state)
	if removed {
		if found["dependent-reply"] != nil {
			t.Fatal("withdrawn discovery-dependent answer survived", found["dependent-reply"])
		}
	} else if found["dependent-reply"]["basisCorrected"] != true || found["dependent-reply"]["discoveryReferences"] == nil {
		t.Fatal("old discovery answer was current or lost its withdrawal dependency", found["dependent-reply"])
	}
	if found["dependent-user"] == nil || found["dependent-user"]["basisCorrected"] != nil || found["independent-reply"] == nil || found["independent-reply"]["basisCorrected"] != nil || found["invite"] == nil {
		t.Fatal("original user message, unrelated answer or invitation changed", found)
	}
	goals, _ := objects(state["goals"])
	methods, _ := objects(goals[0]["approvedMethods"])
	if goals[0]["step"] != "提前五分钟" || methods[1]["text"] != "提早出门" {
		t.Fatal("accepted action changed", goals)
	}
}

func TestDiscoveryReferencesExternalEdits(t *testing.T) {
	for _, operation := range []string{"correct", "confirm", "forget", "source"} {
		t.Run(operation, func(t *testing.T) {
			s, user, path := openFirstReading(t, discoveryDependencyFixture(t))
			var err error
			switch operation {
			case "correct":
				_, err = s.Correct(user, "f", "展厅八点关门。", 2)
			case "confirm":
				_, err = s.Correct(user, "f", "展厅七点关门。", 2)
			case "forget":
				err = s.Forget(user, "f")
			case "source":
				err = s.RemoveSource(user, "s", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			s, err = OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			assertDiscoveryDependents(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object), operation == "forget" || operation == "source")
		})
	}
}

func TestDiscoveryReferencesCASInvalidation(t *testing.T) {
	for _, operation := range []string{"replace", "retry", "revise", "confirm-mode", "citation-quote", "citation-key", "citation-provenance", "no-result"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _ := openFirstReading(t, discoveryDependencyFixture(t))
			defer s.Close()
			read := bridgeRequest(t, s, object{"op": "read"})
			state := read["state"].(object)
			task := state["reveal"].(object)["firstReading"].(object)
			result := task["result"].(object)
			refs, _ := objects(result["citations"])
			switch operation {
			case "replace":
				task["id"] = "reading-2"
			case "retry":
				task["attemptId"] = "attempt-2"
			case "revise":
				task["revision"] = 2
				result["title"] = "本人刚更正的判断"
			case "confirm-mode":
				task["status"] = "corrected"
			case "citation-quote":
				refs[0]["quote"] = "新的引用"
			case "citation-key":
				refs[0]["key"] = "r:copy:0:7"
			case "citation-provenance":
				refs[0]["provenance"] = "user-correction"
			case "no-result":
				delete(task, "result")
				task["status"] = "running"
			}
			write := object{"op": "write", "revision": read["revision"], "base": read["snapshot"], "state": state}
			first := bridgeRequest(t, s, write)
			if retry := bridgeRequest(t, s, write); encode(first) != encode(retry) {
				t.Fatal("CAS retry was not idempotent", first, retry)
			}
			assertDiscoveryDependents(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object), false)
		})
	}
}

func TestDiscoveryReferencesRetainWithdrawalIndexAcrossReplacement(t *testing.T) {
	s, user, _ := openFirstReading(t, discoveryDependencyFixture(t))
	defer s.Close()
	if _, err := s.Correct(user, "f", "展厅八点关门。", 2); err != nil {
		t.Fatal(err)
	}
	read := bridgeRequest(t, s, object{"op": "read"})
	state := read["state"].(object)
	state["reveal"].(object)["firstReading"] = object{"id": "new-reading", "status": "ready", "selection": []any{}}
	bridgeRequest(t, s, object{"op": "write", "revision": read["revision"], "base": read["snapshot"], "state": state})
	assertDiscoveryDependents(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object), false)
	if err := s.RemoveSource(user, "s", nil); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryDependents(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object), true)
}

func TestCurrentDiscoveryReferenceAndLegacyRevisionCAS(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy-zero"}[legacy], func(t *testing.T) {
			state := discoveryDependencyFixture(t)
			task := state["reveal"].(object)["firstReading"].(object)
			dep := discoveryMessages(t, state)["dependent-reply"]["discoveryReferences"].([]any)[0].(object)
			if legacy {
				delete(task, "attemptId")
				delete(dep, "attemptId")
				delete(task, "revision")
				delete(dep, "revision")
			}
			s, _, _ := openFirstReading(t, state)
			defer s.Close()
			read := bridgeRequest(t, s, object{"op": "read"})
			state = read["state"].(object)
			state["name"] = "新的显示名"
			bridgeRequest(t, s, object{"op": "write", "revision": read["revision"], "base": read["snapshot"], "state": state})
			found := discoveryMessages(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object))
			if found["dependent-reply"]["basisCorrected"] != nil || found["dependent-reply"]["discoveryReferences"] == nil {
				t.Fatal("current dependency was changed", found["dependent-reply"])
			}
		})
	}
}

func TestLegacyOpeningDependenciesAreStaleButOrdinaryRawAnswersRemain(t *testing.T) {
	state := discoveryDependencyFixture(t)
	ref := object{"key": "r:s:2:9", "sourceId": "s", "quote": "展厅七点关门。"}
	via := object{"key": "r:s:2:9", "sourceId": "s", "quote": "展厅七点关门。", "via": "proposed-opening"}
	state["conversation"] = append(state["conversation"].([]any),
		object{"id": "legacy-opening", "role": "assistant", "referenceKeys": []any{"opening"}, "readingReferences": []any{ref}},
		object{"id": "legacy-via", "role": "assistant", "readingReferences": []any{via}},
		object{"id": "ordinary-raw", "role": "assistant", "readingReferences": []any{ref}})
	s, _, _ := openFirstReading(t, state)
	defer s.Close()
	found := discoveryMessages(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object))
	if found["legacy-opening"]["basisCorrected"] != true || found["legacy-via"]["basisCorrected"] != true || found["ordinary-raw"]["basisCorrected"] != nil {
		t.Fatal("legacy migration marked the wrong messages", found)
	}
}

// These states were exported by the actual JS reading/chat modules. This
// catches field-location drift that hand-written Go fixtures cannot detect.
func TestDiscoveryBrowserContractAndExternalCorrection(t *testing.T) {
	raw, err := os.ReadFile("testdata/discovery_references.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := parseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"proposed", "corrected"} {
		t.Run(name, func(t *testing.T) {
			state := fixture[name].(object)
			s, user, path := openFirstReading(t, state)
			read := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
			messages := discoveryMessages(t, read)
			expected := "proposed-answer"
			if name == "corrected" {
				expected = "confirmed-answer"
			}
			if messages[expected]["basisCorrected"] != nil || messages["raw-answer"]["basisCorrected"] != nil {
				t.Fatal("JS current identity did not survive Go", messages)
			}
			if name == "corrected" {
				task := read["reveal"].(object)["firstReading"].(object)
				facts, _ := objects(read["facts"])
				var fact object
				for _, f := range facts {
					if str(f, "id") == str(task, "correctionId") {
						fact = f
					}
				}
				v, _ := integer(fact["version"])
				if _, err = s.Correct(user, str(fact, "id"), "方石由访客暂放。", v); err != nil {
					t.Fatal(err)
				}
				s.Close()
				s, err = OpenStore(path)
				if err != nil {
					t.Fatal(err)
				}
				read = bridgeRequest(t, s, object{"op": "read"})["state"].(object)
				messages = discoveryMessages(t, read)
				if messages[expected]["basisCorrected"] != true || messages["raw-answer"]["basisCorrected"] != nil {
					t.Fatal("external unsourced correction affected wrong answers", messages)
				}
				if messages["proposed-answer-q"] == nil || messages["raw-answer-q"] == nil {
					t.Fatal("user turns lost", messages)
				}
			}
			s.Close()
		})
	}
}

func TestDiscoveryCitationSetAndWithdrawalCAS(t *testing.T) {
	raw, err := os.ReadFile("testdata/discovery_references.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"reorder", "duplicate", "withdraw"} {
		t.Run(operation, func(t *testing.T) {
			fixture, err := parseObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			s, _, _ := openFirstReading(t, fixture["proposed"].(object))
			defer s.Close()
			read := bridgeRequest(t, s, object{"op": "read"})
			state := read["state"].(object)
			reply := discoveryMessages(t, state)["proposed-answer"]
			refs, _ := objects(reply["discoveryReferences"])
			citations, _ := objects(refs[0]["citations"])
			if len(citations) != 2 {
				t.Fatal("fixture needs two independent passages")
			}
			switch operation {
			case "reorder":
				refs[0]["citations"] = []any{citations[1], citations[0]}
			case "duplicate":
				refs[0]["citations"] = []any{citations[0], citations[0]}
			case "withdraw":
				state["sources"] = []any{}
			}
			bridgeRequest(t, s, object{"op": "write", "revision": read["revision"], "base": read["snapshot"], "state": state})
			messages := discoveryMessages(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object))
			switch operation {
			case "reorder":
				if messages["proposed-answer"]["basisCorrected"] != nil {
					t.Fatal("citation ordering changed identity")
				}
			case "duplicate":
				if messages["proposed-answer"]["basisCorrected"] != true {
					t.Fatal("duplicate citation accepted")
				}
			case "withdraw":
				if messages["proposed-answer"] != nil || messages["proposed-answer-q"] == nil {
					t.Fatal("withdrawal kept derived answer or lost question", messages)
				}
			}
		})
	}
}

func TestDiscoveryUncitedInputWithdrawalPreservesMarkedAnswer(t *testing.T) {
	state := discoveryDependencyFixture(t)
	task := state["reveal"].(object)["firstReading"].(object)
	task["selection"] = append(task["selection"].([]any), object{"key": "r:unrelated:0:14", "version": 1, "sourceId": "unrelated", "start": 0, "end": 14, "quote": "我习惯星期六整理画笔。", "provenance": "self-quote"})
	s, user, _ := openFirstReading(t, state)
	defer s.Close()
	if err := s.RemoveSource(user, "unrelated", nil); err != nil {
		t.Fatal(err)
	}
	assertDiscoveryDependents(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object), false)
}

func TestDiscoveryLegacyConversationAncestors(t *testing.T) {
	for _, kind := range []string{"m:", "u:"} {
		t.Run(kind, func(t *testing.T) {
			state := discoveryDependencyFixture(t)
			dep := discoveryMessages(t, state)["dependent-reply"]
			dep["referenceKeys"] = []any{"opening"}
			dep["readingReferences"] = []any{object{"key": "r:s:2:9", "version": 1, "sourceId": "s", "quote": "展厅七点关门。"}}
			delete(dep, "discoveryReferences")
			// Keep an actual preceding user/answer pair for the u: reference.
			original, _ := objects(state["conversation"])
			messages := []any{}
			for _, m := range original {
				if str(m, "id") == "dependent-reply" {
					messages = append(messages, object{"id": "ancestor-question", "role": "user", "text": "为什么？"})
				}
				messages = append(messages, m)
			}
			key := kind + "dependent-reply"
			if kind == "u:" {
				key = "u:ancestor-question"
			}
			state["conversation"] = append(messages, object{"id": "legacy-descendant", "role": "assistant", "text": "旧解释的后续答复。", "referenceKeys": []any{key, "reading:r:s:2:9"}, "readingReferences": dep["readingReferences"]}, object{"id": "cycle-a", "role": "assistant", "text": "A", "referenceKeys": []any{"m:cycle-b"}}, object{"id": "cycle-b", "role": "assistant", "text": "B", "referenceKeys": []any{"m:cycle-a"}}, object{"id": "missing", "role": "assistant", "text": "missing", "referenceKeys": []any{"m:absent"}})
			s, _, _ := openFirstReading(t, state)
			defer s.Close()
			found := discoveryMessages(t, bridgeRequest(t, s, object{"op": "read"})["state"].(object))
			for _, id := range []string{"legacy-descendant", "cycle-a", "cycle-b", "missing"} {
				if found[id]["basisCorrected"] != true {
					t.Fatal("legacy ancestor remained current", id, found[id])
				}
			}
			if found["ancestor-question"]["basisCorrected"] != nil || found["independent-reply"]["basisCorrected"] != nil {
				t.Fatal("unrelated/user changed")
			}
		})
	}
}
