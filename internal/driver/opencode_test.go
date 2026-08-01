package driver

import "testing"

func TestCleanRunOutputDropsTheBanner(t *testing.T) {
	raw := "> build · glm-5.2\n\nI'll check the project for recent notes.\n! permission requested: external_directory; auto-rejecting\n"
	got := cleanRunOutput(raw)
	want := "I'll check the project for recent notes.\n! permission requested: external_directory; auto-rejecting"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

// A leading blank line must not hide the banner from the check.
func TestCleanRunOutputSkipsLeadingBlanks(t *testing.T) {
	if got := cleanRunOutput("\n\n> build · glm-5.2\nhello"); got != "hello" {
		t.Errorf("got %q", got)
	}
}

// A reply that happens to start with a quote is not a banner.
func TestCleanRunOutputKeepsRealContent(t *testing.T) {
	if got := cleanRunOutput("the build passed\n> and here is why"); got != "the build passed\n> and here is why" {
		t.Errorf("stripped real content: %q", got)
	}
}

func TestCleanRunOutputEmpty(t *testing.T) {
	if got := cleanRunOutput("> build · glm-5.2\n\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
