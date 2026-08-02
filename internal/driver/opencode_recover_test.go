package driver

import (
	"strings"
	"testing"
)

// realLeak is opencode 1.18.11's actual stdout for a tool-using turn, taken
// verbatim from a reply this driver filed as the agent's answer. The escape
// codes and the "$ " command echo are the whole problem in one sample.
const realLeak = "\x1b[0m\n" +
	"> build · glm-5.2\n" +
	"\x1b[0m\n" +
	"I'll check commits across the sub-repos for the last 2 days (today is 2026-08-01).\n" +
	"\x1b[0m$ \x1b[0mcd \"/repo/omni-analyst\" && for r in app-v2 software; do git -C \"$r\" log; done\n" +
	"========== app-v2 ==========\n" +
	"2026-08-01 509041f docs: completion plan & handoff\n" +
	"2026-08-01 6cd9047 feat(arguments): min_calendar_days\n" +
	"\n" +
	"All activity in the last 2 days is in app-v2 — the other repos are quiet.\n"

func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[0m\x1b[1;32mhello\x1b[0m"); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
	if got := stripANSI("no codes here"); got != "no codes here" {
		t.Errorf("plain text altered: %q", got)
	}
}

// The banner arrives behind an escape sequence. TrimSpace does not remove
// escape characters, so the "> " prefix test used to miss and the banner —
// plus every code in the output — reached the UI.
func TestCleanReplyStripsBannerBehindANSI(t *testing.T) {
	got := cleanReply(realLeak)
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape sequences survived: %q", got[:60])
	}
	if strings.Contains(got, "build · glm-5.2") {
		t.Errorf("banner survived: %q", got[:60])
	}
	if !strings.HasPrefix(got, "I'll check commits") {
		t.Errorf("reply does not start at the agent's first words: %q", got[:60])
	}
}

// A recovered transcript is not a reply, and saying so is the point. It keeps
// the prose, drops the shell command and its output, and labels itself.
func TestRecoveredReplyDropsCommandsAndSaysWhatItIs(t *testing.T) {
	got := recoveredReply(realLeak)

	for _, gone := range []string{"\x1b", "build · glm-5.2", "git -C", "509041f", "========== app-v2"} {
		if strings.Contains(got, gone) {
			t.Errorf("transcript noise survived (%q):\n%s", gone, got)
		}
	}
	for _, want := range []string{"I'll check commits", "All activity in the last 2 days", "recovered from terminal output"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// The whole terminal output of a tool-using turn ran to 12k characters, which
// went into the king's context and a sidebar preview.
func TestRecoveredReplyIsCapped(t *testing.T) {
	got := recoveredReply(strings.Repeat("a line of prose that is not a command\n", 200))
	if len([]rune(got)) > maxRecovered+200 {
		t.Errorf("recovered text is %d runes, cap is %d", len([]rune(got)), maxRecovered)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation is silent")
	}
}

// Nothing worth showing means nothing shown, so the caller falls through to
// reporting the export error rather than filing an empty turn.
func TestRecoveredReplyEmptyWhenOnlyNoise(t *testing.T) {
	if got := recoveredReply("\x1b[0m\n> build · glm-5.2\n\x1b[0m\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// The banner check must not eat a reply that happens to open with a quote.
func TestCleanReplyKeepsQuotedOpening(t *testing.T) {
	in := "the build passed\n> and here is why"
	if got := cleanReply(in); got != in {
		t.Errorf("stripped real content: %q", got)
	}
}
