package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A model preparation is a user-started, one-shot draft over the current
// FirstContext passages. The browser owns the preview and the explicit start;
// this file only keeps the persisted record honest: exact citation keys and
// quotes, bounded sizes, no stale result presented as current, and no silent
// revival after a correction or withdrawal.
const (
	mpMaxRequest      = 4000
	mpMaxQuote        = 2000
	mpMaxInputs       = 40
	mpMaxResult       = 48 * 1024
	mpMaxStoredResult = 64 * 1024
	mpMaxUserEdit     = 20000
	mpMaxMessages     = 48000
	mpMaxPreparation  = 256 * 1024
)

var (
	mpSecrets = regexp.MustCompile(`(?i)密码|口令|密钥|验证码|校验码|授权码|助记词|恢复码|安全码|私钥|\b(?:otp|pin|password|passwd|passcode|bearer|api[ _-]?key|secret[ _-]?key|private[ _-]?key|access[ _-]?token|refresh[ _-]?token|recovery (?:code|phrase)|seed phrase|verification code|security code|one[ -]time (?:code|password))\b|\b(?:sk-[\w-]{12,}|gh[pousr]_[\w]{15,}|AKIA[A-Z0-9]{16}|eyJ[\w-]{8,}\.[\w-]+\.[\w-]+)\b|\b(?:your|login|sign[ -]?in) code\b|\btoken\s*[:=]`)
	mpPrivate = regexp.MustCompile(`(?i)别(?:把.{0,18})?记|不要(?:把.{0,18})?记|别写进画像|不要写进画像|不要存|只聊这一次|don['’]t (?:save|remember|store)|do not (?:save|remember|store)|off the record`)
	mpHTML    = regexp.MustCompile(`(?i)<[a-z!/][^>]*>`)
	mpLink    = regexp.MustCompile(`(?i)(?:https?://|javascript:|data:|file:)`)
)

type mpSelectionShape struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type mpGoalShape struct {
	Title string `json:"title"`
	Step  string `json:"step"`
	Unit  string `json:"unit"`
}

type mpConfigShape struct {
	ArchiveID  string             `json:"archiveId"`
	GoalID     string             `json:"goalId"`
	Request    string             `json:"request"`
	Goal       mpGoalShape        `json:"goal"`
	Selections []mpSelectionShape `json:"selections"`
	Inputs     []string           `json:"inputs"`
}

type mpInputHashShape struct {
	Key         string `json:"key"`
	Version     int    `json:"version"`
	Quote       string `json:"quote"`
	SourceID    any    `json:"sourceId"`
	Start       any    `json:"start"`
	End         any    `json:"end"`
	ContentHash string `json:"contentHash"`
}

type mpBriefShape struct {
	Request      string             `json:"request"`
	MaterialRefs []mpSelectionShape `json:"materialRefs"`
	Goal         mpGoalShape        `json:"goal"`
}

// mpJSON matches JSON.stringify for ordinary text: no HTML escaping and no
// added spaces. Map keys are sorted, so callers that must reproduce a fixed
// JavaScript key order build the object with a struct or an explicit array.
func mpJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func mpNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func mpHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func mpVersion(v any) int {
	if v == nil {
		return 0
	}
	n, err := integer(v)
	if err != nil {
		return 0
	}
	return n
}

func mpStringOrNil(o object, key string) any {
	if value := str(o, key); value != "" {
		return value
	}
	return nil
}

func mpIntOrNil(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	n, err := integer(v)
	if err != nil {
		return nil, err
	}
	return n, nil
}

func mpDocumentIDs(o object) ([]string, error) {
	value, ok := o["documentIds"]
	if !ok || value == nil {
		return []string{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, ErrInvalid
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		id, ok := item.(string)
		if !ok || id == "" || len(id) > 200 {
			return nil, ErrInvalid
		}
		out = append(out, id)
	}
	return out, nil
}

func mpSelections(rows []object) []mpSelectionShape {
	out := make([]mpSelectionShape, 0, len(rows))
	for _, row := range rows {
		kind := str(row, "kind")
		if kind == "" {
			kind = "fact"
		}
		out = append(out, mpSelectionShape{Kind: kind, ID: str(row, "id"), Version: mpVersion(row["version"])})
	}
	return out
}

func mpIdentity(in object) (string, error) {
	version, err := integer(in["version"])
	if err != nil {
		return "", err
	}
	start, err := mpIntOrNil(in["start"])
	if err != nil {
		return "", err
	}
	end, err := mpIntOrNil(in["end"])
	if err != nil {
		return "", err
	}
	documents, err := mpDocumentIDs(in)
	if err != nil {
		return "", err
	}
	sourceBacked, ok := in["sourceBacked"].(bool)
	if !ok {
		return "", ErrInvalid
	}
	return mpJSON([]any{
		str(in, "key"), mpStringOrNil(in, "id"), version, str(in, "quote"),
		str(in, "kind"), str(in, "provenance"), sourceBacked,
		mpStringOrNil(in, "sourceId"), start, end, documents, str(in, "statedAt"),
	}), nil
}

func mpInputHashRows(inputs []object) []mpInputHashShape {
	out := make([]mpInputHashShape, 0, len(inputs))
	for _, in := range inputs {
		start, _ := mpIntOrNil(in["start"])
		end, _ := mpIntOrNil(in["end"])
		out = append(out, mpInputHashShape{
			Key: str(in, "key"), Version: mpVersion(in["version"]), Quote: str(in, "quote"),
			SourceID: mpStringOrNil(in, "sourceId"), Start: start, End: end,
			ContentHash: str(in, "contentHash"),
		})
	}
	return out
}

func mpGoalConfig(goal object) mpGoalShape {
	return mpGoalShape{Title: str(goal, "title"), Step: str(goal, "step"), Unit: str(goal, "unit")}
}

func mpGoalFrom(o object) mpGoalShape {
	goal, _ := o["goal"].(object)
	if goal == nil {
		return mpGoalShape{}
	}
	return mpGoalShape{Title: str(goal, "title"), Step: str(goal, "step"), Unit: str(goal, "unit")}
}

func mpSameGoal(a, b any) bool {
	left, ok := a.(object)
	if !ok || left == nil {
		return false
	}
	right, ok := b.(object)
	if !ok || right == nil {
		return false
	}
	return str(left, "title") == str(right, "title") &&
		str(left, "step") == str(right, "step") &&
		str(left, "unit") == str(right, "unit")
}

func mpValidateGoal(value any) (mpGoalShape, error) {
	o, ok := value.(object)
	if !ok || o == nil {
		return mpGoalShape{}, ErrInvalid
	}
	for key := range o {
		if key != "title" && key != "step" && key != "unit" {
			return mpGoalShape{}, ErrInvalid
		}
	}
	title, step, unit := str(o, "title"), str(o, "step"), str(o, "unit")
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return mpGoalShape{}, ErrInvalid
	}
	if utf8.RuneCountInString(step) > 2000 || utf8.RuneCountInString(unit) > 40 {
		return mpGoalShape{}, ErrInvalid
	}
	if mpSecrets.MatchString(title) || mpSecrets.MatchString(step) {
		return mpGoalShape{}, ErrInvalid
	}
	return mpGoalShape{Title: title, Step: step, Unit: unit}, nil
}

func mpBriefHashForGoal(goal object) string {
	brief, _ := goal["brief"].(object)
	request := ""
	refs := []mpSelectionShape{}
	if brief != nil {
		request = str(brief, "request")
		rows, _ := objects(brief["materialRefs"])
		refs = mpSelections(rows)
	}
	return firstReadingHash("brief", mpJSON(mpBriefShape{Request: request, MaterialRefs: refs, Goal: mpGoalConfig(goal)}))
}

// mpMessagesJSON reproduces JSON.stringify of the two fixed message objects,
// whose keys are always role then content.
func mpMessagesJSON(messages []any) (string, error) {
	if len(messages) != 2 {
		return "", ErrInvalid
	}
	first, ok := messages[0].(object)
	if !ok || len(first) != 2 {
		return "", ErrInvalid
	}
	second, ok := messages[1].(object)
	if !ok || len(second) != 2 {
		return "", ErrInvalid
	}
	return "[{\"role\":" + mpJSON(str(first, "role")) + ",\"content\":" + mpJSON(str(first, "content")) +
		"},{\"role\":" + mpJSON(str(second, "role")) + ",\"content\":" + mpJSON(str(second, "content")) + "}]", nil
}

func mpScalar(value string, max int) error {
	if value == "" || utf8.RuneCountInString(value) > max {
		return ErrInvalid
	}
	if mpHTML.MatchString(value) || mpLink.MatchString(value) || mpSecrets.MatchString(value) {
		return ErrInvalid
	}
	return nil
}

func mpScalarAllowEmpty(value string, max int) error {
	if utf8.RuneCountInString(value) > max {
		return ErrInvalid
	}
	if value != "" && (mpHTML.MatchString(value) || mpLink.MatchString(value) || mpSecrets.MatchString(value)) {
		return ErrInvalid
	}
	return nil
}

func mpValidateRequest(value string) error {
	if value == "" || utf8.RuneCountInString(value) > mpMaxRequest {
		return ErrInvalid
	}
	if mpSecrets.MatchString(value) || mpPrivate.MatchString(value) || mpHTML.MatchString(value) || mpLink.MatchString(value) {
		return ErrInvalid
	}
	return nil
}

func mpLoopback(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || host == "::1" || host == "0:0:0:0:0:0:0:1" {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	numbers := [4]int{}
	for i, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return false
		}
		numbers[i] = n
	}
	return numbers[0] == 127 || numbers[0] == 10 ||
		numbers[0] == 192 && numbers[1] == 168 ||
		numbers[0] == 172 && numbers[1] >= 16 && numbers[1] <= 31
}

func mpValidateDestination(value any) (object, error) {
	o, ok := value.(object)
	if !ok || o == nil {
		return nil, ErrInvalid
	}
	mode, endpoint, model := str(o, "mode"), str(o, "endpoint"), str(o, "model")
	if mode != "system" && mode != "cloud" && mode != "lan" {
		return nil, ErrInvalid
	}
	if model == "" || len(model) > 200 {
		return nil, ErrInvalid
	}
	if mode == "system" {
		if endpoint != "system" {
			return nil, ErrInvalid
		}
		return o, nil
	}
	if endpoint == "" || len(endpoint) > 2048 {
		return nil, ErrInvalid
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrInvalid
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(scheme == "http" && mpLoopback(parsed.Hostname())) {
		return nil, ErrInvalid
	}
	return o, nil
}

func mpSameDestination(a, b any) bool {
	left, ok := a.(object)
	if !ok || left == nil {
		return false
	}
	right, ok := b.(object)
	if !ok || right == nil {
		return false
	}
	return str(left, "mode") == str(right, "mode") &&
		str(left, "endpoint") == str(right, "endpoint") &&
		str(left, "model") == str(right, "model")
}

func mpValidateSelection(sel object) error {
	kind := str(sel, "kind")
	if kind != "fact" && kind != "source" && kind != "discovery" {
		return ErrInvalid
	}
	if str(sel, "id") == "" || len(str(sel, "id")) > 200 {
		return ErrInvalid
	}
	if _, err := integer(sel["version"]); err != nil {
		return ErrInvalid
	}
	return nil
}

func mpValidateSelections(value any) ([]object, error) {
	rows, err := objects(value)
	if err != nil || len(rows) < 1 || len(rows) > mpMaxInputs {
		return nil, ErrInvalid
	}
	for _, row := range rows {
		if err := mpValidateSelection(row); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func mpValidateInput(in object) error {
	key := str(in, "key")
	if key == "" || len(key) > 200 {
		return ErrInvalid
	}
	if _, err := integer(in["version"]); err != nil {
		return ErrInvalid
	}
	quote := str(in, "quote")
	if quote == "" || utf8.RuneCountInString(quote) > mpMaxQuote {
		return ErrInvalid
	}
	if mpSecrets.MatchString(quote) || mpPrivate.MatchString(quote) {
		return ErrInvalid
	}
	if str(in, "kind") == "" || len(str(in, "kind")) > 40 {
		return ErrInvalid
	}
	if str(in, "provenance") == "" || len(str(in, "provenance")) > 60 {
		return ErrInvalid
	}
	sourceBacked, ok := in["sourceBacked"].(bool)
	if !ok {
		return ErrInvalid
	}
	if !mpHex(str(in, "contentHash")) || str(in, "contentHash") != firstReadingHash("passage", quote) {
		return ErrInvalid
	}
	if _, err := mpDocumentIDs(in); err != nil {
		return ErrInvalid
	}
	identity, err := mpIdentity(in)
	if err != nil || identity != str(in, "identity") {
		return ErrInvalid
	}
	if sourceBacked {
		if str(in, "sourceId") == "" || len(str(in, "sourceId")) > 200 {
			return ErrInvalid
		}
		start, err := integer(in["start"])
		if err != nil {
			return ErrInvalid
		}
		end, err := integer(in["end"])
		if err != nil || end <= start {
			return ErrInvalid
		}
	} else {
		if str(in, "id") == "" || len(str(in, "id")) > 200 {
			return ErrInvalid
		}
		if in["start"] != nil || in["end"] != nil {
			return ErrInvalid
		}
	}
	return nil
}

func mpValidateInputs(value any) ([]object, error) {
	rows, err := objects(value)
	if err != nil || len(rows) < 1 || len(rows) > mpMaxInputs {
		return nil, ErrInvalid
	}
	for _, row := range rows {
		if err := mpValidateInput(row); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// A stale version or an invalidated preview keeps no reusable passage text.
// Its input list must therefore be empty rather than merely unvalidated.
func mpValidateInputsEmpty(value any) ([]object, error) {
	rows, err := objects(value)
	if err != nil || len(rows) != 0 {
		return nil, ErrInvalid
	}
	return rows, nil
}

func mpCheckHashes(o object, inputs []object) error {
	selections, err := objects(o["selections"])
	if err != nil {
		return ErrInvalid
	}
	config := mpConfigShape{
		ArchiveID: str(o, "archiveId"), GoalID: str(o, "goalId"), Request: str(o, "request"),
		Goal:       mpGoalFrom(o),
		Selections: mpSelections(selections), Inputs: make([]string, 0, len(inputs)),
	}
	for _, in := range inputs {
		identity, err := mpIdentity(in)
		if err != nil {
			return ErrInvalid
		}
		config.Inputs = append(config.Inputs, identity)
	}
	if firstReadingHash("config", mpJSON(config)) != str(o, "configHash") {
		return ErrInvalid
	}
	if firstReadingHash("input", mpJSON(mpInputHashRows(inputs))) != str(o, "inputHash") {
		return ErrInvalid
	}
	return nil
}

func mpValidateResult(result object, inputs []object) error {
	allowed := map[string]bool{
		"kind": true, "title": true, "summary": true, "sections": true, "supported": true,
		"assumptions": true, "unknowns": true, "questions": true,
		"resultId": true, "attemptId": true, "receivedAt": true, "inputHash": true,
		"configHash": true, "destination": true, "rawHash": true,
	}
	for key := range result {
		if !allowed[key] {
			return ErrInvalid
		}
	}
	kind := str(result, "kind")
	if kind != "draft" && kind != "comparison" && kind != "checklist" {
		return ErrInvalid
	}
	if err := mpScalar(str(result, "title"), 160); err != nil {
		return err
	}
	if err := mpScalarAllowEmpty(str(result, "summary"), 600); err != nil {
		return err
	}
	sections, err := objects(result["sections"])
	if err != nil || len(sections) < 1 || len(sections) > 8 {
		return ErrInvalid
	}
	itemCount := 0
	for _, section := range sections {
		for key := range section {
			if key != "heading" && key != "items" {
				return ErrInvalid
			}
		}
		if err := mpScalar(str(section, "heading"), 120); err != nil {
			return err
		}
		items, ok := section["items"].([]any)
		if !ok || len(items) < 1 || len(items) > 20 {
			return ErrInvalid
		}
		itemCount += len(items)
		if itemCount > 60 {
			return ErrInvalid
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return ErrInvalid
			}
			if err := mpScalar(text, 1200); err != nil {
				return err
			}
		}
	}
	supported, err := objects(result["supported"])
	if err != nil || len(supported) < 1 || len(supported) > 20 {
		return ErrInvalid
	}
	quotes := map[string]string{}
	for _, in := range inputs {
		quotes[str(in, "key")] = str(in, "quote")
	}
	for _, item := range supported {
		for key := range item {
			if key != "text" && key != "refs" {
				return ErrInvalid
			}
		}
		if err := mpScalar(str(item, "text"), 1200); err != nil {
			return err
		}
		refs, err := objects(item["refs"])
		if err != nil || len(refs) < 1 || len(refs) > 6 {
			return ErrInvalid
		}
		for _, ref := range refs {
			for key := range ref {
				if key != "key" && key != "quote" {
					return ErrInvalid
				}
			}
			key, quote := str(ref, "key"), str(ref, "quote")
			if key == "" || quote == "" || quotes[key] != quote {
				return ErrInvalid
			}
		}
	}
	for _, field := range []string{"assumptions", "unknowns", "questions"} {
		items, ok := result[field].([]any)
		if !ok || len(items) > 20 {
			return ErrInvalid
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return ErrInvalid
			}
			if err := mpScalar(text, 600); err != nil {
				return err
			}
		}
	}
	if str(result, "resultId") == "" || len(str(result, "resultId")) > 200 {
		return ErrInvalid
	}
	if str(result, "attemptId") == "" || len(str(result, "attemptId")) > 200 {
		return ErrInvalid
	}
	if str(result, "receivedAt") == "" {
		return ErrInvalid
	}
	if !mpHex(str(result, "inputHash")) || !mpHex(str(result, "configHash")) || !mpHex(str(result, "rawHash")) {
		return ErrInvalid
	}
	if _, err := mpValidateDestination(result["destination"]); err != nil {
		return ErrInvalid
	}
	if len(mpJSON(result)) > mpMaxStoredResult {
		return ErrInvalid
	}
	return nil
}

func mpValidatePreview(pv object, goalID string) error {
	allowed := map[string]bool{
		"id": true, "goalId": true, "archiveId": true, "request": true, "goal": true,
		"destination": true, "selections": true, "inputs": true, "excluded": true,
		"coverage": true, "briefHash": true, "configHash": true, "inputHash": true,
		"messagesHash": true, "payload": true, "messages": true, "createdAt": true,
		"invalid": true, "invalidReason": true, "consumed": true,
	}
	for key := range pv {
		if !allowed[key] {
			return ErrInvalid
		}
	}
	if str(pv, "id") == "" || str(pv, "goalId") != goalID || str(pv, "archiveId") == "" {
		return ErrInvalid
	}
	if err := mpValidateRequest(str(pv, "request")); err != nil {
		return err
	}
	if _, err := mpValidateGoal(pv["goal"]); err != nil {
		return err
	}
	if _, err := mpValidateSelections(pv["selections"]); err != nil {
		return err
	}
	if !mpHex(str(pv, "briefHash")) || !mpHex(str(pv, "configHash")) || !mpHex(str(pv, "inputHash")) {
		return ErrInvalid
	}
	invalid, ok := pv["invalid"].(bool)
	if !ok {
		return ErrInvalid
	}
	consumed, ok := pv["consumed"].(bool)
	if !ok {
		return ErrInvalid
	}
	hasDestination := pv["destination"] != nil
	if hasDestination {
		if _, err := mpValidateDestination(pv["destination"]); err != nil {
			return err
		}
	}
	if invalid {
		// An invalidated preview must not keep a reusable copy of the passages
		// or of the approved message. Only identity and hashes remain.
		if _, err := mpValidateInputsEmpty(pv["inputs"]); err != nil {
			return err
		}
		payload, err := objects(pv["payload"])
		if err != nil || len(payload) != 0 {
			return ErrInvalid
		}
		if pv["messages"] != nil || str(pv, "messagesHash") != "" || !consumed {
			return ErrInvalid
		}
	} else {
		inputs, err := mpValidateInputs(pv["inputs"])
		if err != nil {
			return err
		}
		if err := mpCheckHashes(pv, inputs); err != nil {
			return err
		}
		payload, err := objects(pv["payload"])
		if err != nil || len(payload) != len(inputs) {
			return ErrInvalid
		}
		for i, item := range payload {
			if str(item, "key") != str(inputs[i], "key") || str(item, "quote") != str(inputs[i], "quote") {
				return ErrInvalid
			}
		}
		if len(mpJSON(payload)) > mpMaxMessages {
			return ErrInvalid
		}
		if !hasDestination {
			if pv["messages"] != nil || str(pv, "messagesHash") != "" {
				return ErrInvalid
			}
		} else {
			messages, ok := pv["messages"].([]any)
			if !ok {
				return ErrInvalid
			}
			encoded, err := mpMessagesJSON(messages)
			if err != nil {
				return err
			}
			first := messages[0].(object)
			second := messages[1].(object)
			if str(first, "role") != "system" || str(first, "content") == "" || len(str(first, "content")) > 8000 {
				return ErrInvalid
			}
			if str(second, "role") != "user" {
				return ErrInvalid
			}
			var parsed []any
			dec := json.NewDecoder(strings.NewReader(str(second, "content")))
			dec.UseNumber()
			if err := dec.Decode(&parsed); err != nil || len(parsed) < 2 {
				return ErrInvalid
			}
			envelope, ok := parsed[0].(object)
			if !ok || len(envelope) != 3 {
				return ErrInvalid
			}
			if str(envelope, "key") != "__request__" || str(envelope, "request") != str(pv, "request") {
				return ErrInvalid
			}
			if !mpSameGoal(envelope["goal"], pv["goal"]) {
				return ErrInvalid
			}
			if mpJSON(parsed[1:]) != mpJSON(payload) {
				return ErrInvalid
			}
			if len(encoded) > mpMaxMessages {
				return ErrInvalid
			}
			if !mpHex(str(pv, "messagesHash")) || firstReadingHash("messages", encoded) != str(pv, "messagesHash") {
				return ErrInvalid
			}
		}
	}
	excluded, err := objects(pv["excluded"])
	if err != nil {
		return ErrInvalid
	}
	for _, item := range excluded {
		if str(item, "kind") == "" || str(item, "id") == "" || str(item, "reason") == "" {
			return ErrInvalid
		}
	}
	if _, ok := pv["coverage"].(object); !ok {
		return ErrInvalid
	}
	if str(pv, "createdAt") == "" {
		return ErrInvalid
	}
	if reason, ok := pv["invalidReason"]; ok && reason != nil {
		if text, ok := reason.(string); !ok || len(text) > 2000 {
			return ErrInvalid
		}
	}
	if len(mpJSON(pv)) > mpMaxPreparation {
		return ErrInvalid
	}
	return nil
}

func mpValidateCurrent(cur object, goalID string, mp object) error {
	allowed := map[string]bool{
		"version": true, "id": true, "attemptId": true, "previewId": true, "status": true,
		"archiveId": true, "goalId": true, "request": true, "goal": true, "destination": true,
		"selections": true, "inputs": true, "briefHash": true, "configHash": true,
		"inputHash": true, "messagesHash": true, "startedAt": true, "finishedAt": true,
		"result": true, "userEdit": true, "saved": true, "error": true, "staleReason": true,
		"staleAt": true, "resultId": true, "staleInputs": true,
	}
	for key := range cur {
		if !allowed[key] {
			return ErrInvalid
		}
	}
	if str(cur, "goalId") != goalID || str(cur, "archiveId") == "" {
		return ErrInvalid
	}
	if str(cur, "id") == "" || str(cur, "attemptId") == "" || str(cur, "previewId") == "" {
		return ErrInvalid
	}
	status := str(cur, "status")
	if status != "running" && status != "complete" && status != "failed" && status != "cancelled" && status != "stale" {
		return ErrInvalid
	}
	if err := mpValidateRequest(str(cur, "request")); err != nil {
		return err
	}
	if _, err := mpValidateGoal(cur["goal"]); err != nil {
		return err
	}
	if _, err := mpValidateDestination(cur["destination"]); err != nil {
		return err
	}
	if _, err := mpValidateSelections(cur["selections"]); err != nil {
		return err
	}
	if !mpHex(str(cur, "briefHash")) || !mpHex(str(cur, "configHash")) || !mpHex(str(cur, "inputHash")) {
		return ErrInvalid
	}
	if str(cur, "startedAt") == "" {
		return ErrInvalid
	}
	if len(str(cur, "error")) > 2000 || len(str(cur, "staleReason")) > 2000 {
		return ErrInvalid
	}
	if status == "stale" {
		// A stale version keeps only identity hashes for dependency propagation.
		// The passages, generated result and user edit are cleared so a withdrawn
		// source cannot be re-read from a derived copy.
		if _, err := mpValidateInputsEmpty(cur["inputs"]); err != nil {
			return err
		}
		if str(cur, "messagesHash") != "" || cur["result"] != nil || cur["userEdit"] != nil || cur["saved"] != nil {
			return ErrInvalid
		}
		if resultID, ok := cur["resultId"]; ok && resultID != nil {
			if text, ok := resultID.(string); !ok || text != "" {
				return ErrInvalid
			}
		}
		stale, err := objects(cur["staleInputs"])
		if err != nil || len(stale) > mpMaxInputs {
			return ErrInvalid
		}
		for _, in := range stale {
			for key := range in {
				if key != "key" && key != "sourceId" && key != "id" && key != "version" && key != "contentHash" {
					return ErrInvalid
				}
			}
			if str(in, "key") == "" {
				return ErrInvalid
			}
			if _, err := integer(in["version"]); err != nil {
				return ErrInvalid
			}
			if !mpHex(str(in, "contentHash")) {
				return ErrInvalid
			}
			for _, field := range []string{"sourceId", "id"} {
				if value, ok := in[field]; ok && value != nil {
					text, ok := value.(string)
					if !ok || text == "" || len(text) > 200 {
						return ErrInvalid
					}
				}
			}
		}
		if len(mpJSON(cur)) > mpMaxPreparation {
			return ErrInvalid
		}
		return nil
	}
	inputs, err := mpValidateInputs(cur["inputs"])
	if err != nil {
		return err
	}
	if !mpHex(str(cur, "messagesHash")) {
		return ErrInvalid
	}
	if err := mpCheckHashes(cur, inputs); err != nil {
		return err
	}
	if resultID, ok := cur["resultId"]; ok && resultID != nil {
		if text, ok := resultID.(string); !ok || len(text) > 200 {
			return ErrInvalid
		}
	}
	result, hasResult := cur["result"].(object)
	if hasResult && result != nil {
		if err := mpValidateResult(result, inputs); err != nil {
			return err
		}
		if str(result, "attemptId") != str(cur, "attemptId") ||
			str(result, "inputHash") != str(cur, "inputHash") ||
			str(result, "configHash") != str(cur, "configHash") ||
			!mpSameDestination(result["destination"], cur["destination"]) {
			return ErrInvalid
		}
		if status == "running" {
			return ErrInvalid
		}
	} else if status == "complete" {
		return ErrInvalid
	}
	if raw, ok := cur["userEdit"]; ok && raw != nil {
		edit, ok := raw.(object)
		if !ok {
			return ErrInvalid
		}
		text := str(edit, "text")
		if text == "" || utf8.RuneCountInString(text) > mpMaxUserEdit {
			return ErrInvalid
		}
		mode := str(edit, "mode")
		if str(edit, "at") == "" || (mode != "user" && mode != "derived-edit") || str(edit, "basedOnResultId") == "" {
			return ErrInvalid
		}
		if _, ok := edit["citationsFromResult"].(bool); !ok {
			return ErrInvalid
		}
		if added, ok := edit["added"]; ok && added != nil {
			if value, ok := added.(string); !ok || utf8.RuneCountInString(value) > mpMaxUserEdit {
				return ErrInvalid
			}
		}
	}
	if raw, ok := cur["saved"]; ok && raw != nil {
		saved, ok := raw.(object)
		if !ok {
			return ErrInvalid
		}
		mode := str(saved, "mode")
		if str(saved, "at") == "" || (mode != "system" && mode != "user") {
			return ErrInvalid
		}
	}
	if pv, ok := mp["preview"].(object); ok && pv != nil && str(pv, "id") == str(cur, "previewId") {
		if messages, ok := pv["messages"].([]any); ok && messages != nil {
			encoded, err := mpMessagesJSON(messages)
			if err != nil {
				return err
			}
			if firstReadingHash("messages", encoded) != str(cur, "messagesHash") {
				return ErrInvalid
			}
		}
	}
	if len(mpJSON(cur)) > mpMaxPreparation {
		return ErrInvalid
	}
	return nil
}

func mpValidateModelPreparation(mp object, goalID string) error {
	for key := range mp {
		if key != "version" && key != "preview" && key != "current" {
			return ErrInvalid
		}
	}
	version, err := integer(mp["version"])
	if err != nil || version != 1 {
		return ErrInvalid
	}
	if raw, present := mp["preview"]; present && raw != nil {
		pv, ok := raw.(object)
		if !ok {
			return ErrInvalid
		}
		if err := mpValidatePreview(pv, goalID); err != nil {
			return err
		}
	}
	if raw, present := mp["current"]; present && raw != nil {
		cur, ok := raw.(object)
		if !ok {
			return ErrInvalid
		}
		if err := mpValidateCurrent(cur, goalID, mp); err != nil {
			return err
		}
	}
	if len(mpJSON(mp)) > mpMaxPreparation {
		return ErrInvalid
	}
	return nil
}

func mpValidateBrief(goal object) error {
	brief, ok := goal["brief"].(object)
	if !ok || brief == nil {
		return nil
	}
	mp, ok := brief["modelPreparation"].(object)
	if !ok || mp == nil {
		return nil
	}
	request := str(brief, "request")
	refs, err := objects(brief["materialRefs"])
	if err != nil {
		return ErrInvalid
	}
	if request == "" && len(refs) == 0 {
		// Cancelling the material brief keeps only the stale preparation. It
		// can be read, but no new preview can be authorized from it.
		if cur, ok := mp["current"].(object); ok && cur != nil && str(cur, "status") != "stale" {
			return ErrInvalid
		}
		return nil
	}
	if request == "" || len(refs) == 0 {
		return ErrInvalid
	}
	if utf8.RuneCountInString(request) > mpMaxRequest {
		return ErrInvalid
	}
	for _, ref := range refs {
		if err := mpValidateSelection(ref); err != nil {
			return err
		}
	}
	return nil
}

// validateModelPreparations runs on every dossier write. It never repairs a
// malformed record silently; the write is rejected instead.
func validateModelPreparations(d *dossierRows) error {
	for _, goal := range d.rows["goals"] {
		if err := mpValidateBrief(goal); err != nil {
			return err
		}
		brief, _ := goal["brief"].(object)
		if brief == nil {
			continue
		}
		mp, _ := brief["modelPreparation"].(object)
		if mp == nil {
			continue
		}
		if err := mpValidateModelPreparation(mp, str(goal, "id")); err != nil {
			return err
		}
	}
	return nil
}

func mpFact(d *dossierRows, id string) object {
	for _, fact := range d.rows["facts"] {
		if str(fact, "id") == id {
			return fact
		}
	}
	return nil
}

func mpFactSpans(fact object) []object {
	spans, _ := objects(fact["evidence"])
	if len(spans) > 0 {
		return spans
	}
	if str(fact, "sourceId") != "" {
		return []object{{
			"sourceId": str(fact, "sourceId"), "start": fact["start"], "end": fact["end"],
			"quote": str(fact, "quote"),
		}}
	}
	return nil
}

func mpIntersects(aStart, aEnd int, b object) bool {
	start, err := integer(b["start"])
	if err != nil {
		return true
	}
	end, err := integer(b["end"])
	if err != nil {
		return true
	}
	return aStart < end && start < aEnd
}

func mpInputCurrent(d *dossierRows, in object) bool {
	quote := str(in, "quote")
	if quote == "" || str(in, "contentHash") != firstReadingHash("passage", quote) {
		return false
	}
	if mpSecrets.MatchString(quote) || mpPrivate.MatchString(quote) {
		return false
	}
	version, err := integer(in["version"])
	if err != nil || version < 1 {
		return false
	}
	sourceBacked, ok := in["sourceBacked"].(bool)
	if !ok {
		return false
	}
	if sourceBacked {
		sourceID := str(in, "sourceId")
		source, ok := d.sources[sourceID]
		if !ok {
			return false
		}
		kind := str(source, "kind")
		if kind != "self" && kind != "mail" && kind != "file" {
			return false
		}
		body := str(source, "text")
		if body == "" || mpPrivate.MatchString(body) || mpSecrets.MatchString(body) {
			return false
		}
		start, err := integer(in["start"])
		if err != nil {
			return false
		}
		end, err := integer(in["end"])
		if err != nil || end <= start {
			return false
		}
		clone := maps.Clone(in)
		if err := d.indexes.convertSpan(clone, d.sources, true, true); err != nil {
			return false
		}
		for _, span := range d.rows["dismissed"] {
			if str(span, "sourceId") == sourceID && mpIntersects(start, end, span) {
				return false
			}
		}
		for _, fact := range d.rows["facts"] {
			if fact["confirmed"] != true {
				continue
			}
			for _, span := range mpFactSpans(fact) {
				if str(span, "sourceId") != sourceID {
					continue
				}
				clone := maps.Clone(span)
				if err := d.indexes.convertSpan(clone, d.sources, true, true); err != nil {
					return false
				}
				if mpIntersects(start, end, span) {
					return false
				}
			}
		}
		if id := str(in, "id"); id != "" {
			fact := mpFact(d, id)
			if fact == nil {
				return false
			}
			factVersion, err := integer(fact["version"])
			if err != nil || factVersion != version {
				return false
			}
			if str(fact, "text") != quote || fact["confirmed"] == true {
				return false
			}
			matched := false
			for _, span := range mpFactSpans(fact) {
				if str(span, "sourceId") != sourceID {
					continue
				}
				spanStart, err := integer(span["start"])
				if err != nil {
					continue
				}
				spanEnd, err := integer(span["end"])
				if err != nil {
					continue
				}
				if spanStart == start && spanEnd == end && str(span, "quote") == quote {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		} else {
			for _, fact := range d.rows["facts"] {
				if fact["confirmed"] == true {
					continue
				}
				for _, span := range mpFactSpans(fact) {
					if str(span, "sourceId") != sourceID {
						continue
					}
					spanStart, err := integer(span["start"])
					if err != nil {
						continue
					}
					spanEnd, err := integer(span["end"])
					if err != nil {
						continue
					}
					if spanStart == start && spanEnd == end && str(span, "quote") == quote {
						return false
					}
				}
			}
		}
		return true
	}
	id := str(in, "id")
	if id == "" {
		return false
	}
	fact := mpFact(d, id)
	if fact == nil {
		return false
	}
	factVersion, err := integer(fact["version"])
	if err != nil || factVersion != version {
		return false
	}
	if str(fact, "text") != quote {
		return false
	}
	if fact["confirmed"] == true {
		for _, span := range mpFactSpans(fact) {
			source, ok := d.sources[str(span, "sourceId")]
			if !ok {
				return false
			}
			if mpPrivate.MatchString(str(source, "text")) {
				return false
			}
			if mpSecrets.MatchString(str(span, "quote")) || mpPrivate.MatchString(str(span, "quote")) {
				return false
			}
			clone := maps.Clone(span)
			if err := d.indexes.convertSpan(clone, d.sources, true, true); err != nil {
				return false
			}
			start, err := integer(span["start"])
			if err != nil {
				return false
			}
			end, err := integer(span["end"])
			if err != nil {
				return false
			}
			for _, dismissed := range d.rows["dismissed"] {
				if str(dismissed, "sourceId") == str(span, "sourceId") && mpIntersects(start, end, dismissed) {
					return false
				}
			}
		}
		return true
	}
	return fact["retainedReading"] == true && str(fact, "sourceId") == ""
}

func mpInputsCurrent(d *dossierRows, value any) bool {
	inputs, err := objects(value)
	if err != nil || len(inputs) == 0 {
		return false
	}
	for _, in := range inputs {
		if !mpInputCurrent(d, in) {
			return false
		}
	}
	return true
}

func mpClearPreview(pv object, reason string) {
	pv["invalid"] = true
	pv["invalidReason"] = reason
	pv["consumed"] = true
	pv["inputs"] = []any{}
	pv["payload"] = []any{}
	pv["messages"] = nil
	pv["messagesHash"] = ""
}

func mpClearStale(cur object, reason string) {
	cur["status"] = "stale"
	cur["staleReason"] = reason
	cur["staleAt"] = mpNow()
	cur["error"] = reason
	inputs, _ := objects(cur["inputs"])
	stale := make([]any, 0, len(inputs))
	for _, in := range inputs {
		stale = append(stale, object{
			"key": str(in, "key"), "sourceId": mpStringOrNil(in, "sourceId"),
			"id": mpStringOrNil(in, "id"), "version": in["version"],
			"contentHash": str(in, "contentHash"),
		})
	}
	cur["staleInputs"] = stale
	cur["inputs"] = []any{}
	cur["result"] = nil
	cur["resultId"] = ""
	cur["userEdit"] = nil
	cur["saved"] = nil
	cur["messagesHash"] = ""
}

// mpStaleArtifactXML mirrors the browser's stale card: it keeps the request
// the user approved but drops every derived quote, result and edit copy.
func mpStaleArtifactXML(artifact object, cur object) string {
	proactive := objectOrEmpty(artifact["proactive"])
	kind := str(proactive, "kind")
	if kind == "" {
		kind = "model-preparation"
	}
	reason := str(cur, "staleReason")
	if reason == "" {
		reason = "参考资料、目标或要求已经变化，这版准备不能继续使用。"
	}
	return `<prepared id="` + xmlEscape(str(artifact, "id")) + `" kind="` + xmlEscape(kind) + `" status="stale" tint="grape">` +
		`<title>模型准备已过期</title>` +
		`<why>` + xmlEscape(reason) + `</why>` +
		`<request>` + xmlEscape(str(cur, "request")) + `</request>` +
		`<change>` + xmlEscape("过期于 "+str(cur, "staleAt")+"；旧结果和引用副本已清除。") + `</change></prepared>`
}

// mpStaleArtifact clears the card that mirrors a stale model preparation.
// A server-side withdrawal or correction must not leave a ready card with the
// old text while waiting for a client prune that may never run.
func mpStaleArtifact(state object, goalID string, cur object) bool {
	artifacts := objectsOrEmpty(state["artifacts"])
	changed := false
	for _, artifact := range artifacts {
		proactive := objectOrEmpty(artifact["proactive"])
		if str(proactive, "kind") != "model-preparation" {
			continue
		}
		if str(artifact, "goalId") != goalID &&
			str(proactive, "key") != "model-preparation:"+goalID &&
			str(artifact, "id") != "modelprep-"+goalID {
			continue
		}
		proactive["status"] = "stale"
		artifact["proactive"] = proactive
		artifact["xml"] = mpStaleArtifactXML(artifact, cur)
		plan := objectOrEmpty(artifact["plan"])
		plan["resultId"] = ""
		plan["systemCopy"] = ""
		plan["textMode"] = "stale"
		delete(plan, "text")
		delete(plan, "userNote")
		artifact["plan"] = plan
		changed = true
	}
	if changed {
		state["artifacts"] = toAnyObjects(artifacts)
	}
	return changed
}

// normalizeModelPreparations marks a whole version stale when any input is no
// longer current. It never un-stales: restoring an old source cannot revive a
// synthesis that was already invalidated. Invalidating also clears the derived
// passage and result copies, keeping only identity hashes.
func normalizeModelPreparations(d *dossierRows) {
	archiveID := str(d.fields, "archiveId")
	for _, goal := range d.rows["goals"] {
		brief, ok := goal["brief"].(object)
		if !ok || brief == nil {
			continue
		}
		mp, ok := brief["modelPreparation"].(object)
		if !ok || mp == nil {
			continue
		}
		if pv, ok := mp["preview"].(object); ok && pv != nil && pv["invalid"] != true {
			reason := ""
			if str(pv, "archiveId") != archiveID {
				reason = "存档已经切换，请重新打开预览。"
			} else if str(pv, "briefHash") != mpBriefHashForGoal(goal) {
				reason = "资料、目标或存档已经变化，请重新打开预览。"
			} else if !mpInputsCurrent(d, pv["inputs"]) {
				reason = "资料、目标或存档已经变化，请重新打开预览。"
			}
			if reason != "" {
				mpClearPreview(pv, reason)
			}
		}
		if cur, ok := mp["current"].(object); ok && cur != nil {
			status := str(cur, "status")
			if status == "complete" || status == "running" {
				reason := ""
				if str(cur, "archiveId") != archiveID {
					reason = "存档已经切换，这版准备不能继续使用。"
				} else if str(cur, "briefHash") != mpBriefHashForGoal(goal) {
					reason = "准备要求或材料范围已经变化，整版准备已过期。"
				} else if !mpInputsCurrent(d, cur["inputs"]) {
					reason = "选中的资料已经撤回、更正或不再可用，整版准备已过期。"
				}
				if reason != "" {
					mpClearStale(cur, reason)
					mpStaleArtifact(d.fields, str(goal, "id"), cur)
				}
			}
		}
	}
}

func mpIDSet(id string) map[string]bool {
	out := map[string]bool{}
	if id != "" {
		out[id] = true
	}
	return out
}

func mpInputAffected(in object, factIDs, sourceIDs, documents map[string]bool,
	documentHash func(string) (string, error)) (bool, error) {
	if factIDs[str(in, "id")] || factIDs[strings.TrimPrefix(str(in, "key"), "f:")] {
		return true, nil
	}
	sourceID := str(in, "sourceId")
	if sourceIDs[sourceID] {
		return true, nil
	}
	if sourceID != "" && documentHash != nil {
		hash, err := documentHash(sourceID)
		if err != nil {
			return false, err
		}
		if hash != "" && documents[hash] {
			return true, nil
		}
	}
	return false, nil
}

// androidInvalidateModelPreparations is the service-side half of the same
// contract: a corrected or withdrawn fact/source makes the whole prepared
// version stale, including a request that is still in flight.
func androidInvalidateModelPreparations(tx *sql.Tx, user string, factIDs, sourceIDs, documents map[string]bool,
	documentHash func(string) (string, error)) error {
	rows, err := tx.Query("SELECT id,extra FROM android_fields WHERE user_id=? AND kind='goals'", user)
	if err != nil {
		return err
	}
	type goalRow struct{ id, extra string }
	var goals []goalRow
	staleGoals := map[string]object{}
	for rows.Next() {
		var row goalRow
		if err := rows.Scan(&row.id, &row.extra); err != nil {
			rows.Close()
			return err
		}
		goals = append(goals, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, row := range goals {
		fields, err := parseObject([]byte(row.extra))
		if err != nil {
			return err
		}
		brief, _ := fields["brief"].(object)
		if brief == nil {
			continue
		}
		mp, _ := brief["modelPreparation"].(object)
		if mp == nil {
			continue
		}
		changed := false
		if cur, ok := mp["current"].(object); ok && cur != nil {
			status := str(cur, "status")
			if status == "complete" || status == "running" {
				inputs, err := objects(cur["inputs"])
				if err != nil {
					return err
				}
				affected := false
				for _, in := range inputs {
					hit, err := mpInputAffected(in, factIDs, sourceIDs, documents, documentHash)
					if err != nil {
						return err
					}
					if hit {
						affected = true
						break
					}
				}
				if affected {
					mpClearStale(cur, "参考资料已更正或撤回，这版模型准备已过期。")
					staleGoals[row.id] = cur
					changed = true
				}
			}
		}
		if pv, ok := mp["preview"].(object); ok && pv != nil && pv["invalid"] != true {
			inputs, err := objects(pv["inputs"])
			if err != nil {
				return err
			}
			affected := false
			for _, in := range inputs {
				hit, err := mpInputAffected(in, factIDs, sourceIDs, documents, documentHash)
				if err != nil {
					return err
				}
				if hit {
					affected = true
					break
				}
			}
			if affected {
				mpClearPreview(pv, "参考资料已更正或撤回，请重新打开预览。")
				changed = true
			}
		}
		if !changed {
			continue
		}
		if _, err := tx.Exec("UPDATE android_fields SET extra=? WHERE user_id=? AND kind='goals' AND id=?",
			encode(fields), user, row.id); err != nil {
			return err
		}
	}
	if len(staleGoals) == 0 {
		return nil
	}
	// Cards live in the dossier metadata, not in the goals rows. Clear them in
	// the same transaction so a server-side withdrawal never leaves a ready
	// card with the old text until some client happens to prune it.
	var raw sql.NullString
	if err := tx.QueryRow("SELECT metadata FROM android_dossier WHERE user_id=?", user).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	metadata := object{}
	if raw.Valid {
		metadata, err = parseObject([]byte(raw.String))
		if err != nil {
			return err
		}
	}
	cleared := false
	for goalID, cur := range staleGoals {
		if mpStaleArtifact(metadata, goalID, cur) {
			cleared = true
		}
	}
	if !cleared {
		return nil
	}
	_, err = tx.Exec("UPDATE android_dossier SET metadata=? WHERE user_id=?", encode(metadata), user)
	return err
}
