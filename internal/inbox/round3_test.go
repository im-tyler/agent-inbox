package inbox

// Round-3 audit regressions (short-prompt re-audit, pinned to the round-2
// merge commit).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/im-tyler/agent-inbox/internal/driver"
	"golang.org/x/sys/unix"
)

type auditTargetDriver struct{ called chan string }

func (d auditTargetDriver) Name() string                       { return "mock" }
func (d auditTargetDriver) AttachArgs(string, string) []string { return nil }
func (d auditTargetDriver) Send(_ context.Context, dir, _, _ string) driver.Result {
	d.called <- dir
	return driver.Result{Final: "fixture", Status: driver.StatusWaiting}
}

// F01: a numeric send must not change targets between resolving the project
// and acquiring its claim. The claim lock makes the interleaving
// deterministic: the first resolution happens, the list shifts, then the
// lock opens.
func TestAuditNumericSendDoesNotSwitchProjects(t *testing.T) {
	base := t.TempDir()
	lockDir := filepath.Join(base, "claims", ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(lockDir, "alpha.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)

	d := auditTargetDriver{called: make(chan string, 2)}
	in := New([]*Project{
		{Name: "alpha", Tool: "mock", Dir: filepath.Join(base, "alpha")},
		{Name: "beta", Tool: "mock", Dir: filepath.Join(base, "beta")},
	}, map[string]driver.Driver{"mock": d}, filepath.Join(base, "state.json"))
	t.Cleanup(in.Close)

	selected := make(chan struct{})
	finished := make(chan error, 1)
	first := true
	go func() {
		_, err := in.startSend(func() (*Project, error) {
			p, err := in.project(1)
			if first {
				first = false
				close(selected)
			}
			return p, err
		}, "for alpha", "for alpha", true)
		finished <- err
	}()

	select {
	case <-selected:
	case <-time.After(3 * time.Second):
		t.Fatal("initial resolution did not happen")
	}
	in.mu.Lock()
	in.projects = in.projects[1:] // alpha removed; beta becomes index 1
	in.mu.Unlock()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("send accepted a target that disappeared during acquisition")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send did not finish")
	}
	select {
	case dir := <-d.called:
		t.Fatalf("driver unexpectedly invoked in %s", dir)
	default:
	}
	if _, held := in.claims.Peek("alpha"); held {
		t.Fatal("original alpha claim was leaked")
	}
}

// F03: startup must restore the change counter and the event-dedup markers.
func TestAuditLoadStateRestoresRevisionAndSeenEvents(t *testing.T) {
	dir := t.TempDir()
	saved := Project{
		Name: "alpha", Tool: "claude", Dir: dir,
		SessionID: "fixture-session", Status: driver.StatusWaiting,
		UpdatedAt: time.Unix(100, 0), Revision: 7,
		SeenEvents: []string{"event-1.json"},
	}
	data, err := json.Marshal([]Project{saved})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := &Project{Name: saved.Name, Tool: saved.Tool, Dir: saved.Dir}
	LoadState(path, []*Project{got})
	if got.Revision != saved.Revision {
		t.Errorf("revision = %d, want %d", got.Revision, saved.Revision)
	}
	if !reflect.DeepEqual(got.SeenEvents, saved.SeenEvents) {
		t.Errorf("seen events = %v, want %v", got.SeenEvents, saved.SeenEvents)
	}
}

// F02: a stale frontend must not send with a cached identity after another
// process changed the project's tool.
func TestAuditStaleFrontendRefusesToSend(t *testing.T) {
	env := newMultiEnv(t)
	cfgPath := filepath.Join(env.base, "config.json")
	alpha := filepath.Join(env.base, "alpha")

	// The current configuration says codex; the long-lived inbox loaded claude.
	cfg := `{"projects": [{"name": "alpha", "tool": "codex", "dir": "` + alpha + `"}]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	in := New(
		[]*Project{{Name: "alpha", Tool: "claude", Dir: alpha, Status: driver.StatusIdle, SessionID: "s1"}},
		map[string]driver.Driver{"claude": &recDriver{seedWith: "s1"}, "codex": &recDriver{}},
		env.state,
	).WithConfigPath(cfgPath)
	t.Cleanup(in.Close)

	if out, ok := in.SendAndWait("alpha", "hello", 5*time.Second); ok || out.Err == nil {
		t.Fatalf("stale frontend sent: ok=%v err=%v", ok, out.Err)
	}
	if got := in.drivers["claude"].(*recDriver); len(got.received()) != 0 {
		t.Fatal("the old tool's driver was invoked against the current configuration")
	}

	// A frontend whose definitions match sends normally.
	fresh := New(
		[]*Project{{Name: "alpha", Tool: "codex", Dir: alpha, Status: driver.StatusIdle}},
		map[string]driver.Driver{"claude": &recDriver{}, "codex": &recDriver{seedWith: "s2"}},
		env.state,
	).WithConfigPath(cfgPath)
	t.Cleanup(fresh.Close)
	if out, ok := fresh.SendAndWait("alpha", "hello", 15*time.Second); !ok || out.Err != nil {
		t.Fatalf("current frontend send failed: ok=%v err=%v", ok, out.Err)
	}
}
