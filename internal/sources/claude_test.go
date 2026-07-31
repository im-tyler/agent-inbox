package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentinbox/internal/feed"
)

var fixedNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

func claude(t *testing.T, agentsJSON string) Claude {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat <<'JSON'\n" + agentsJSON + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Claude{Bin: bin, Root: filepath.Join(dir, "projects"), Now: func() time.Time { return fixedNow }}
}

func fetch(t *testing.T, c Claude) []feed.Item {
	t.Helper()
	f, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f.Items
}

const blockedBg = `[{"id":"ad19117b","sessionId":"ad19117b-67a4-4ad0-a333-e39fcc756240",
  "cwd":"/repos/teploy","kind":"background","name":"resume-background-agent",
  "status":"idle","state":"blocked","startedAt":1783971598326}]`

func TestClaudeCodesOwnBlockedStateIsTrustedRatherThanRederived(t *testing.T) {
	items := fetch(t, claude(t, blockedBg))
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0].State != feed.StateBlocked || items[0].Attention != "" && items[0].Needs == nil {
		t.Fatalf("expected a blocked item with needs, got %+v", items[0])
	}
	if items[0].Context["kind"] != "background" || items[0].Context["project"] != "teploy" {
		t.Fatalf("context lost detail: %+v", items[0].Context)
	}
}

func TestABackgroundAgentIsAttachedToNotResumed(t *testing.T) {
	// Claude Code refuses --resume on a live session: "currently running as a
	// background agent (bg)". Offering resume here produced exit status 1 and
	// was the whole reason this source stopped guessing at state.
	actions := fetch(t, claude(t, blockedBg))[0].Needs.Actions
	if len(actions) != 2 {
		t.Fatalf("expected attach and fork, got %+v", actions)
	}
	if actions[0].Label != "attach" {
		t.Fatalf("attach must lead, got %q", actions[0].Label)
	}
	joined := strings.Join(actions[0].Run, " ")
	if joined != "claude agents --cwd /repos/teploy" {
		t.Fatalf("attach should open the agent view filtered to the project, got %q", joined)
	}
	if actions[0].Dir != "/repos/teploy" {
		t.Fatalf("attach should run in the project, got %q", actions[0].Dir)
	}
	// Fork always works and does not disturb the running session.
	if strings.Join(actions[1].Run, " ") != "claude --resume ad19117b-67a4-4ad0-a333-e39fcc756240 --fork-session" {
		t.Fatalf("unexpected fork argv: %v", actions[1].Run)
	}
}

func TestABusySessionIsRunningAndOffersNothing(t *testing.T) {
	items := fetch(t, claude(t, `[{"sessionId":"s1","cwd":"/repos/neutron","kind":"interactive",
	  "name":"neutron-79","status":"busy","startedAt":1784915267440}]`))
	if items[0].State != feed.StateRunning {
		t.Fatalf("a busy session is running, got %q", items[0].State)
	}
	if items[0].Needs != nil {
		t.Fatal("nothing is being asked of you while it works")
	}
}

func TestBackgroundAndInteractiveBlocksReadDifferently(t *testing.T) {
	bg := fetch(t, claude(t, blockedBg))[0]
	inter := fetch(t, claude(t, `[{"sessionId":"s1","cwd":"/repos/x","kind":"interactive",
	  "name":"x","state":"blocked"}]`))[0]

	if !strings.Contains(bg.Needs.Prompt, "Background agent") {
		t.Fatalf("a background block should say so, got %q", bg.Needs.Prompt)
	}
	if strings.Contains(inter.Needs.Prompt, "Background") {
		t.Fatalf("an interactive block should not, got %q", inter.Needs.Prompt)
	}
}

func TestTitleFallsBackToTheProjectWhenUnnamed(t *testing.T) {
	items := fetch(t, claude(t, `[{"sessionId":"s1","cwd":"/repos/akiroo","kind":"background"}]`))
	if items[0].Title != "session in akiroo" {
		t.Fatalf("got %q", items[0].Title)
	}
}

func TestSinceFallsBackToStartedAtWhenNoTranscriptIsFound(t *testing.T) {
	items := fetch(t, claude(t, blockedBg))
	want := time.UnixMilli(1783971598326).UTC().Format(time.RFC3339)
	if items[0].Since != want {
		t.Fatalf("got %q, want %q", items[0].Since, want)
	}
}

func TestIDFallsBackToTheShortAgentIDWhenSessionIDIsAbsent(t *testing.T) {
	items := fetch(t, claude(t, `[{"id":"09eac2f6","cwd":"/repos/x","kind":"background"}]`))
	if items[0].ID != "09eac2f6" {
		t.Fatalf("got %q", items[0].ID)
	}
}

func TestNoLiveSessionsIsAnEmptyFeedNotAnError(t *testing.T) {
	if got := len(fetch(t, claude(t, `[]`))); got != 0 {
		t.Fatalf("got %d items", got)
	}
}

func TestAMissingOrFailingClaudeBinaryIsReportedNotSwallowed(t *testing.T) {
	// "claude is not installed" and "you have nothing waiting" mean very
	// different things to whoever is reading the merged list.
	c := Claude{Bin: filepath.Join(t.TempDir(), "nope"), Now: func() time.Time { return fixedNow }}
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestMalformedAgentJSONIsReported(t *testing.T) {
	if _, err := claude(t, `not json`).Fetch(context.Background()); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestEnrichmentAddsBranchAndLastPromptFromTheTranscript(t *testing.T) {
	c := claude(t, `[{"sessionId":"sess-1","cwd":"/repos/akiroo","kind":"background","name":"Polish the UI","state":"blocked"}]`)
	dir := filepath.Join(c.Root, "-repos-akiroo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"gitBranch":"feat/mail","lastPrompt":"keep going","timestamp":"2026-07-31T11:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sess-1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	item := fetch(t, c)[0]
	if item.Context["branch"] != "feat/mail" || item.Context["last_prompt"] != "keep going" {
		t.Fatalf("enrichment missing: %+v", item.Context)
	}
	if item.Since != "2026-07-31T11:00:00Z" {
		t.Fatalf("transcript time should win over startedAt, got %q", item.Since)
	}
}

func TestAnUnreadableTranscriptCostsDetailNotTheItem(t *testing.T) {
	items := fetch(t, claude(t, blockedBg))
	if len(items) != 1 || items[0].Context["branch"] != "" {
		t.Fatalf("expected the item without enrichment, got %+v", items)
	}
}

func TestDecodeAcceptsEnvelopeBareArrayAndEmptyOutput(t *testing.T) {
	f, err := decode("x", []byte(`{"schema":"teploy.inbox/v1","items":[{"id":"a"}]}`))
	if err != nil || len(f.Items) != 1 {
		t.Fatalf("envelope: %v %+v", err, f)
	}
	// Producers get the envelope wrong at first; a readable list beats a
	// strict parser.
	f, err = decode("x", []byte(`[{"id":"a"},{"id":"b"}]`))
	if err != nil || len(f.Items) != 2 {
		t.Fatalf("bare array: %v %+v", err, f)
	}
	if f, err := decode("x", []byte("  \n")); err != nil || len(f.Items) != 0 {
		t.Fatalf("empty: %v %+v", err, f)
	}
}

func TestOneDeadSourceDoesNotBlankTheList(t *testing.T) {
	live := claude(t, blockedBg)
	dead := Exec{Label: "dead", Command: []string{"/nonexistent/binary"}}

	items, results := FetchAll(context.Background(), []Source{live, dead})
	if len(items) != 1 {
		t.Fatalf("the working source should still render, got %d items", len(items))
	}
	if bad := Errors(results); len(bad) != 1 || bad[0].Source != "dead" {
		t.Fatalf("the dead source should report itself, got %+v", bad)
	}
}

func TestFetchAllStampsOriginSoSameIDsFromTwoSourcesCoexist(t *testing.T) {
	items, _ := FetchAll(context.Background(), []Source{claude(t, blockedBg)})
	if items[0].Origin != "claude-code" {
		t.Fatalf("origin should name the configured source, got %q", items[0].Origin)
	}
}
