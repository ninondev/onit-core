package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeopleSurviveAndroidBridgeRoundTrip(t *testing.T) {
	if _, err := os.Stat("../src/web/core.js"); err != nil {
		t.Skip("frontend modules not present in this repository")
	}
	script := `const S=require('../src/web/core.js');const s=S.empty();S.addPerson(s,{name:'小明',relationship:'表哥',note:'喜欢钓鱼'});console.log(JSON.stringify(s));`
	raw, err := exec.Command("node", "-e", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "people.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bridgeRequest(t, s, object{"op": "write", "revision": 0, "state": state})
	saved := bridgeRequest(t, s, object{"op": "read"})["state"].(object)
	people, err := objects(saved["people"])
	if err != nil || len(people) != 1 || str(people[0], "name") != "小明" || str(people[0], "relationship") != "表哥" {
		t.Fatalf("people lost over bridge: %v %v", strings.Join([]string{}, ""), err)
	}
}
