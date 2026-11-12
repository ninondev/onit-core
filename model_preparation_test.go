package main

import (
	"os"
	"strings"
	"testing"
)

// The fixture is produced by the browser module from synthetic material only.
// It proves the Go writer accepts exactly the persisted shape the browser
// creates, including the goal/action snapshot and the message hash.
func loadModelPreparationDossier(t *testing.T) object {
	t.Helper()
	body, err := os.ReadFile("testdata/model-preparation-dossier.json")
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(body)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func modelPreparationGoal(t *testing.T, d *dossierRows) object {
	t.Helper()
	goals := d.rows["goals"]
	if len(goals) != 1 {
		t.Fatalf("goals=%d", len(goals))
	}
	brief, _ := goals[0]["brief"].(object)
	if brief == nil {
		t.Fatal("missing brief")
	}
	mp, _ := brief["modelPreparation"].(object)
	if mp == nil {
		t.Fatal("missing modelPreparation")
	}
	return mp
}

func TestModelPreparationBrowserShapeRoundTrip(t *testing.T) {
	d, err := splitDossier(loadModelPreparationDossier(t))
	if err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	current, _ := mp["current"].(object)
	if current == nil || str(current, "status") != "complete" {
		t.Fatal(current)
	}
	edit, _ := current["userEdit"].(object)
	if edit == nil || str(edit, "mode") != "derived-edit" || str(edit, "added") != "我补一句自己的话。" {
		t.Fatal(edit)
	}
	if goal, _ := current["goal"].(object); goal == nil || str(goal, "title") != "博物馆讲解" {
		t.Fatal("goal snapshot missing")
	}
	if !mpHex(str(current, "messagesHash")) {
		t.Fatal("messages hash missing")
	}
	preview, _ := mp["preview"].(object)
	if preview == nil || preview["consumed"] != true {
		t.Fatal("preview should be consumed after begin")
	}
	artifacts, _ := objects(d.fields["artifacts"])
	if len(artifacts) != 1 {
		t.Fatal(artifacts)
	}
	plan, _ := artifacts[0]["plan"].(object)
	if str(plan, "textMode") != "derived-edit" || str(plan, "userNote") != "我补一句自己的话。" {
		t.Fatal(plan)
	}
}

func TestModelPreparationWithdrawalClearsDerivedCopies(t *testing.T) {
	d, err := splitDossier(loadModelPreparationDossier(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rows["sources"]) != 1 {
		t.Fatalf("sources=%d", len(d.rows["sources"]))
	}
	sourceID := str(d.rows["sources"][0], "id")
	delete(d.sources, sourceID)
	d.rows["sources"] = nil
	normalizeModelPreparations(d)
	if err := validateModelPreparations(d); err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	current, _ := mp["current"].(object)
	if current == nil || str(current, "status") != "stale" {
		t.Fatal(current)
	}
	if inputs, _ := objects(current["inputs"]); len(inputs) != 0 {
		t.Fatal("stale current retained inputs")
	}
	if current["result"] != nil || current["userEdit"] != nil || current["saved"] != nil {
		t.Fatal("stale current retained derived content")
	}
	preview, _ := mp["preview"].(object)
	if preview == nil || preview["invalid"] != true || preview["messages"] != nil {
		t.Fatal(preview)
	}
	if inputs, _ := objects(preview["inputs"]); len(inputs) != 0 {
		t.Fatal("invalid preview retained inputs")
	}
	if str(current, "messagesHash") != "" {
		t.Fatal("stale current retained message hash")
	}
	artifacts, _ := objects(d.fields["artifacts"])
	if len(artifacts) != 1 {
		t.Fatal(artifacts)
	}
	artifact := artifacts[0]
	if status := str(objectOrEmpty(artifact["proactive"]), "status"); status != "stale" {
		t.Fatalf("artifact status=%q", status)
	}
	plan := objectOrEmpty(artifact["plan"])
	if str(plan, "systemCopy") != "" || str(plan, "textMode") != "stale" || plan["text"] != nil || plan["userNote"] != nil {
		t.Fatalf("stale artifact retained derived plan: %v", plan)
	}
	if body := str(artifact, "xml"); strings.Contains(body, "讲解限时 120 秒") || strings.Contains(body, "我补一句自己的话") {
		t.Fatalf("stale artifact XML retained old text: %s", body)
	}
}

func TestModelPreparationSelectedUncitedChangeClearsArtifact(t *testing.T) {
	state := loadModelPreparationDossier(t)
	sources, err := objects(state["sources"])
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources=%d", len(sources))
	}
	// The second selected passage is not cited by the result, but changing it
	// still invalidates the whole synthesis and its mirrored card.
	sources[0]["text"] = "讲解限时 90 秒。"
	state["sources"] = toAnyObjects(sources)
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateModelPreparations(d); err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	current, _ := mp["current"].(object)
	if current == nil || str(current, "status") != "stale" {
		t.Fatal(current)
	}
	artifacts, _ := objects(d.fields["artifacts"])
	if len(artifacts) != 1 {
		t.Fatal(artifacts)
	}
	artifact := artifacts[0]
	if status := str(objectOrEmpty(artifact["proactive"]), "status"); status != "stale" {
		t.Fatalf("artifact status=%q", status)
	}
	if body := str(artifact, "xml"); strings.Contains(body, "全文 330 字") {
		t.Fatalf("stale artifact XML retained changed passage: %s", body)
	}
	plan := objectOrEmpty(artifact["plan"])
	if str(plan, "systemCopy") != "" || plan["text"] != nil || plan["userNote"] != nil {
		t.Fatalf("stale artifact retained derived plan: %v", plan)
	}
}

func TestModelPreparationRevisedPreviewRoundTrip(t *testing.T) {
	body, err := os.ReadFile("testdata/model-preparation-revised.json")
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(body)
	if err != nil {
		t.Fatal(err)
	}
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	preview, _ := mp["preview"].(object)
	if preview == nil || preview["invalid"] != false || preview["consumed"] != false {
		t.Fatal(preview)
	}
	inputs, _ := objects(preview["inputs"])
	if len(inputs) != 1 {
		t.Fatalf("revised inputs=%d", len(inputs))
	}
	if !mpHex(str(preview, "messagesHash")) {
		t.Fatal("revised preview lost messages hash")
	}
}

func TestModelPreparationNoDestinationPreviewRoundTrip(t *testing.T) {
	body, err := os.ReadFile("testdata/model-preparation-nodest.json")
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(body)
	if err != nil {
		t.Fatal(err)
	}
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	preview, _ := mp["preview"].(object)
	if preview == nil || preview["destination"] != nil || preview["messages"] != nil || str(preview, "messagesHash") != "" {
		t.Fatal(preview)
	}
}

func TestModelPreparationRejectsRetainedStaleContent(t *testing.T) {
	d, err := splitDossier(loadModelPreparationDossier(t))
	if err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	current, _ := mp["current"].(object)
	current["status"] = "stale"
	if err := validateModelPreparations(d); err == nil {
		t.Fatal("stale current with retained result was accepted")
	}
}

func TestModelPreparationRunningShapeRoundTrip(t *testing.T) {
	body, err := os.ReadFile("testdata/model-preparation-running.json")
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(body)
	if err != nil {
		t.Fatal(err)
	}
	d, err := splitDossier(state)
	if err != nil {
		t.Fatal(err)
	}
	mp := modelPreparationGoal(t, d)
	current, _ := mp["current"].(object)
	if current == nil || str(current, "status") != "running" {
		t.Fatal(current)
	}
	if str(current, "resultId") != "" || current["result"] != nil {
		t.Fatal("running current should have no result yet")
	}
	preview, _ := mp["preview"].(object)
	if preview == nil || preview["consumed"] != true {
		t.Fatal("preview should be consumed once the attempt started")
	}
	if !mpHex(str(current, "messagesHash")) {
		t.Fatal("running current lost its message hash")
	}
}
