package main

import (
	"testing"
	"time"
)

// The page can already hold other prepared cards today. A goal whose user
// explicitly configured a preparation must still produce its schedule-plan
// card; explicit configuration is never crowded out by other output.
func TestSchedulePlanIsNotBlockedByTodaysOrdinaryCards(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	state := schedulePlanState(now)
	sources := objectsOrEmpty(state["sources"])
	sources[0]["title"] = "合成日程 A"
	state["sources"] = toAnyObjects(sources)
	// Another card already occupies Today; it belongs to a goal that no
	// longer exists, so maintenance retires it in the same pass.
	state["artifacts"] = []any{object{
		"id": "widget:prepared-brief-g2", "type": "prepared", "date": now.Format("2006-01-02"),
		"xml": `<prepared kind="material-brief" status="ready"/>`,
		"proactive": object{"kind": "material-brief", "key": "brief:g2", "status": "ready"},
	}}

	made, changed, err := RunProactive(state, now)
	if err != nil || !changed || len(made) != 1 {
		t.Fatalf("schedule plan made=%d changed=%v err=%v", len(made), changed, err)
	}
	if kind := str(objectOrEmpty(made[0]["proactive"]), "kind"); kind != "schedule-plan" {
		t.Fatalf("kind=%s", kind)
	}
}

// A tick can rebuild a prepared card while its inputs are unchanged (for
// example on app reload). The rebuilt XML is fresh, but the user's own chosen
// slot and edited text must stay attached to the card.
func TestPreparedUserPlanSurvivesRebuild(t *testing.T) {
	chosen := object{"begin": int64(1000), "end": int64(2000), "label": "09:55–10:55"}
	state := object{
		"artifacts": []any{
			object{
				"id": "a1", "type": "prepared",
				"plan": object{
					"text": "我改过的准备：带水。", "textMode": "user",
					"userNote": "我改过的准备：带水。", "systemCopy": "系统原文",
					"kind": "user", "chosenSlot": chosen,
					"textAt": "2026-11-11T10:00:00Z", "systemCopyAt": "2026-11-11T09:59:00Z",
				},
			},
		},
	}
	card := object{"id": "a1", "type": "prepared", "xml": "<prepared/>", "plan": object{}}
	upsertProactiveArtifact(state, card, map[string]object{})

	plan := objectOrEmpty(objectsOrEmpty(state["artifacts"])[0]["plan"])
	for key, want := range map[string]string{
		"text": "我改过的准备：带水。", "textMode": "user",
		"userNote": "我改过的准备：带水。", "systemCopy": "系统原文",
		"kind": "user", "textAt": "2026-11-11T10:00:00Z", "systemCopyAt": "2026-11-11T09:59:00Z",
	} {
		if got := str(plan, key); got != want {
			t.Fatalf("plan[%s]=%q want %q: %s", key, got, want, encode(plan))
		}
	}
	if got := objectOrEmpty(plan["chosenSlot"]); str(got, "label") != "09:55–10:55" {
		t.Fatalf("chosenSlot lost: %s", encode(plan))
	}
}

// A legacy copy has text but no provenance. A rebuild must not copy it back as
// the user's text; only the quarantine fields may survive.
func TestPreparedLegacyPlanIsNotCopiedAsUserTextOnRebuild(t *testing.T) {
	state := object{
		"artifacts": []any{
			object{
				"id": "a1", "type": "prepared",
				"plan": object{"text": "合成旧系统私密标记甲"},
			},
		},
	}
	card := object{"id": "a1", "type": "prepared", "xml": "<prepared/>", "plan": object{}}
	upsertProactiveArtifact(state, card, map[string]object{})

	plan := objectOrEmpty(objectsOrEmpty(state["artifacts"])[0]["plan"])
	if got := str(plan, "text"); got != "" {
		t.Fatalf("legacy text was copied as user text: %q", got)
	}
	if got := str(plan, "textMode"); got != "" {
		t.Fatalf("legacy text gained provenance: %q", got)
	}
}
