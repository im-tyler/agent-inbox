package config

import (
	"path/filepath"
	"testing"
)

// Every new field has to survive the write-read cycle, or a setting the user
// made silently reverts on the next runtime mutation.
func TestNewFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	in := &Settings{
		Projects: []Project{{Name: "a", Tool: "claude", Dir: "/a"}, {Name: "b", Tool: "claude", Dir: "/b"}},
		Groups: []Group{
			{Name: "one", Projects: []string{"a"}, King: GroupKing{Tool: "codex"}},
			{Name: "two", Projects: []string{"b"}},
		},
	}
	in.King.Autonomous = true
	in.King.WakesPerHour = 7
	in.King.Rounds = 3

	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !out.King.Autonomous || out.King.WakesPerHour != 7 || out.King.Rounds != 3 {
		t.Errorf("king settings lost: %+v", out.King)
	}
	if len(out.Groups) != 2 {
		t.Fatalf("groups lost: %+v", out.Groups)
	}
	if out.Groups[0].Name != "one" || out.Groups[0].King.Tool != "codex" ||
		len(out.Groups[0].Projects) != 1 || out.Groups[0].Projects[0] != "a" {
		t.Errorf("group 0 lost detail: %+v", out.Groups[0])
	}
	if err := Validate(out); err != nil {
		t.Errorf("a config we wrote does not validate: %v", err)
	}
}

// AddProject is the runtime mutation path — it loads, edits and saves the
// whole file, so anything it does not understand is what gets dropped.
func TestRuntimeAddPreservesGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := &Settings{
		Projects: []Project{{Name: "a", Tool: "claude", Dir: "/a"}},
		Groups:   []Group{{Name: "one", Projects: []string{"a"}}},
	}
	s.King.Autonomous = true
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(path)
	if !loaded.AddProject(Project{Name: "c", Tool: "claude", Dir: "/c"}) {
		t.Fatal("AddProject refused")
	}
	if err := Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(path)
	if len(again.Groups) != 1 || !again.King.Autonomous {
		t.Errorf("adding a project dropped groups or autonomy: groups=%+v autonomous=%v",
			again.Groups, again.King.Autonomous)
	}
	// The added project belongs to no group, which is legal — it joins the
	// first one at runtime.
	if err := Validate(again); err != nil {
		t.Errorf("an unclaimed project made the config invalid: %v", err)
	}
}
