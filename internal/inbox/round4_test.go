package inbox

// Round-4 audit regressions.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
)

// F01: hand-written configs with relative dirs are refused at validation.
func TestRelativeConfigDirRejected(t *testing.T) {
	s := &config.Settings{Projects: []config.Project{{Name: "a", Tool: "claude", Dir: "some/repo"}}}
	if err := config.Validate(s); err == nil {
		t.Fatal("relative project dir accepted — its meaning changes with the cwd")
	}
	s.Projects[0].Dir = t.TempDir()
	s.King.Dir = "."
	if err := config.Validate(s); err == nil {
		t.Fatal("relative king dir accepted")
	}
	s.King.Dir = ""
	if err := config.Validate(s); err != nil {
		t.Fatalf("absolute config rejected: %v", err)
	}
}

// F01: legacy state with a relative dir does not restore into today's cwd.
func TestLegacyRelativeStateNotRestored(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	b, _ := json.Marshal([]map[string]any{{
		"name": "a", "tool": "mock", "dir": ".", "session_id": "s1",
		"status": "waiting", "updated_at": time.Now().Format(time.RFC3339),
	}})
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Project{Name: "a", Tool: "mock", Dir: dir}
	LoadState(state, []*Project{p})
	if p.SessionID == "s1" {
		t.Fatal("a relative persisted dir restored a session — its repository binding is ambiguous")
	}
}

// F05: Unicode fold pairs that uniqueness treats as distinct resolve distinctly.
func TestUnicodeFoldPairsResolveDistinctly(t *testing.T) {
	env := newMultiEnv(t)
	d1 := filepath.Join(env.base, "one")
	d2 := filepath.Join(env.base, "two")
	for _, d := range []string{d1, d2} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	in := New([]*Project{
		{Name: "s", Tool: "mock", Dir: d1, Status: driver.StatusIdle},
		{Name: "\u017f", Tool: "mock", Dir: d2, Status: driver.StatusIdle},
	}, map[string]driver.Driver{"mock": driver.Mock{}}, env.state)
	t.Cleanup(in.Close)
	in.mu.Lock()
	a, errA := in.projectByName("\u017f")
	in.mu.Unlock()
	if errA != nil {
		t.Fatal(errA)
	}
	if a.Name != "\u017f" {
		t.Fatalf("lookup of the long-s resolved to %q — EqualFold routed across the canonical identity", a.Name)
	}
}

// F06: cancelling work owned by another frontend reports whose it is.
func TestCancelRefusesForeignOwner(t *testing.T) {
	base := t.TempDir()
	sleep := startSleep(t)
	claimsDir := filepath.Join(base, "claims", "alpha")
	if err := os.MkdirAll(claimsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaim(t, claimsDir, sleep.Process.Pid, "foreign")
	in := New([]*Project{{Name: "alpha", Tool: "mock", Dir: filepath.Join(base, "repo"), Status: driver.StatusWorking}},
		map[string]driver.Driver{"mock": driver.Mock{}}, filepath.Join(base, "state.json"))
	t.Cleanup(in.Close)
	if err := in.Cancel(1); err == nil {
		t.Fatal("cancel reported success over another frontend's live turn")
	}
	got, _ := in.Detail(1)
	if got.Status != driver.StatusWorking {
		t.Fatalf("status reset to %q while the owner still runs", got.Status)
	}
}
