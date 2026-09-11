package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet verbs are one process each: build an inbox from the same files
// any other front-end would read, run one operation, close. These tests give
// each one its own data directory with a mock project, the way a harness
// would find them, and assert on what the harness would parse.

func fleetEnv(t *testing.T) string {
	t.Helper()
	dd := t.TempDir()
	dir := filepath.Join(dd, "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"projects": [{"name": "alpha", "tool": "mock", "dir": "` + dir + `"}]}`
	if err := os.WriteFile(filepath.Join(dd, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_INBOX_DIR", dd)
	t.Setenv("AGENT_INBOX_CONFIG", filepath.Join(dd, "config.json"))
	return dd
}

func TestFleetStatusJSON(t *testing.T) {
	fleetEnv(t)
	var out bytes.Buffer
	if err := runFleet([]string{"status", "--json"}, &out, &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	var doc fleetStatusDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("status output is not the documented JSON: %v\n%s", err, out.String())
	}
	// The supervisor and the configured project are both there, and the
	// partition says which is which.
	if len(doc.Projects) != 2 {
		t.Fatalf("projects = %d, want 2 (supervisor + alpha)", len(doc.Projects))
	}
	var sawSupervisor, sawAlpha bool
	for _, p := range doc.Projects {
		if p.Name == "supervisor" {
			sawSupervisor = p.Supervisor
		}
		if p.Name == "alpha" {
			sawAlpha = !p.Supervisor
		}
	}
	if !sawSupervisor || !sawAlpha {
		t.Fatalf("supervisor marking wrong: %+v", doc.Projects)
	}
	if len(doc.Groups) != 1 || doc.Groups[0].King != "supervisor" {
		t.Fatalf("groups = %+v", doc.Groups)
	}
}

func TestFleetSendRoundTrip(t *testing.T) {
	fleetEnv(t)
	var out, errOut bytes.Buffer
	if err := runFleet([]string{"send", "alpha", "hello there"}, &out, &errOut); err != nil {
		t.Fatalf("send: %v (stderr: %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "[mock] handled") {
		t.Fatalf("reply not printed: %q", out.String())
	}

	// The turn persisted: a second process sees the session id, which is the
	// whole difference between a headless send and a fire-and-forget echo.
	var st bytes.Buffer
	if err := runFleet([]string{"status", "--json"}, &st, &st); err != nil {
		t.Fatal(err)
	}
	var doc fleetStatusDoc
	if err := json.Unmarshal(st.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Projects {
		if p.Name == "alpha" {
			if p.LastMessage == "" {
				t.Fatal("send did not persist a reply")
			}
			return
		}
	}
	t.Fatal("alpha missing from status")
}

func TestFleetSendUnknownProjectIsAnError(t *testing.T) {
	fleetEnv(t)
	var out bytes.Buffer
	if err := runFleet([]string{"send", "nobody", "x"}, &out, &out); err == nil {
		t.Fatal("unknown project did not fail the command")
	}
}

func TestFleetNoteLifecycle(t *testing.T) {
	fleetEnv(t)
	var out bytes.Buffer
	if err := runFleet([]string{"note", "add", "teploy depends on neutron's client"}, &out, &out); err != nil {
		t.Fatalf("add: %v", err)
	}
	// A second process sees it: notes are fleet state, not process state.
	out.Reset()
	if err := runFleet([]string{"note", "list"}, &out, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "neutron's client") {
		t.Fatalf("note not listed across processes: %q", out.String())
	}
	out.Reset()
	if err := runFleet([]string{"note", "drop", "neutron's client"}, &out, &out); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if !strings.Contains(out.String(), "1 note(s) dropped") {
		t.Fatalf("drop output: %q", out.String())
	}
}

func TestFleetLogbookAuthorFlag(t *testing.T) {
	fleetEnv(t)
	var out bytes.Buffer
	// The documented form: author after the text. flag.Parse cannot see it
	// (it stops at the first positional), so this is the regression test for
	// the argument splitting.
	if err := runFleet([]string{"logbook", "add", "parked teploy", "--as", "opencode"}, &out, &out); err != nil {
		t.Fatalf("add: %v", err)
	}
	out.Reset()
	if err := runFleet([]string{"logbook"}, &out, &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out.String(), "[") || !strings.Contains(out.String(), "opencode] parked teploy") {
		t.Fatalf("entry or author wrong: %q", out.String())
	}
}

func TestFleetAddProject(t *testing.T) {
	fleetEnv(t)
	dir := filepath.Join(t.TempDir(), "bravo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runFleet([]string{"add", "bravo", "mock", dir}, &out, &out); err != nil {
		t.Fatalf("add: %v", err)
	}
	// It landed in config, which is the durable definition of the fleet.
	b, err := os.ReadFile(filepath.Join(fleetEnvDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"bravo"`) {
		t.Fatalf("bravo not in config: %s", b)
	}
	// And a duplicate is refused.
	out.Reset()
	if err := runFleet([]string{"add", "bravo", "mock", dir}, &out, &out); err == nil {
		t.Fatal("duplicate add accepted")
	}
}

func fleetEnvDir(t *testing.T) string {
	t.Helper()
	return os.Getenv("AGENT_INBOX_DIR")
}

func TestFleetGitQuery(t *testing.T) {
	env := fleetEnv(t)
	// A real repository, because the query is a real subprocess.
	repo := filepath.Join(env, "repo")
	run := func(args ...string) {
		c := exec.Command(args[0], args[1:]...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "t@t")
	run("git", "config", "user.name", "t")
	run("git", "commit", "-q", "--allow-empty", "-m", "first")

	var out bytes.Buffer
	if err := runFleet([]string{"git", "alpha", "log"}, &out, &out); err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out.String(), "first") {
		t.Fatalf("log output missing the commit: %q", out.String())
	}
}
