package driver

import "testing"

func TestCleanReplyDropsTheBanner(t *testing.T) {
	raw := "> build · glm-5.2\n\nI'll check the project for recent notes.\n! permission requested: external_directory; auto-rejecting\n"
	got := cleanReply(raw)
	want := "I'll check the project for recent notes.\n! permission requested: external_directory; auto-rejecting"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A leading blank line must not hide the banner from the check.
func TestCleanReplySkipsLeadingBlanks(t *testing.T) {
	if got := cleanReply("\n\n> build · glm-5.2\nhello"); got != "hello" {
		t.Errorf("got %q", got)
	}
}

// A reply that happens to start with a quote is not a banner.
func TestCleanReplyKeepsRealContent(t *testing.T) {
	if got := cleanReply("the build passed\n> and here is why"); got != "the build passed\n> and here is why" {
		t.Errorf("stripped real content: %q", got)
	}
}

func TestCleanReplyEmpty(t *testing.T) {
	if got := cleanReply("> build · glm-5.2\n\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
