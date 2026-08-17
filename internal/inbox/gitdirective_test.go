package inbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/git"
)

func TestParseGitDirectives(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []GitDirective
	}{
		{
			name: "target and kind",
			in:   "looking now\n[git: neutron status]\n",
			want: []GitDirective{{Target: "neutron", Kind: "status"}},
		},
		{
			name: "bare target defaults to status",
			in:   "[git: neutron]",
			want: []GitDirective{{Target: "neutron", Kind: "status"}},
		},
		{
			name: "several",
			in:   "[git: neutron diff]\n[git: teploy log]",
			want: []GitDirective{{Target: "neutron", Kind: "diff"}, {Target: "teploy", Kind: "log"}},
		},
		{
			name: "case insensitive prefix",
			in:   "[GIT: neutron LOG]",
			want: []GitDirective{{Target: "neutron", Kind: "LOG"}},
		},
		{
			// A directive is a whole line, matching how every other parser here
			// reads them, so prose about the syntax cannot become one.
			name: "mid-sentence mention is not a directive",
			in:   "you can write [git: neutron status] to ask me directly",
			want: nil,
		},
		{
			name: "unterminated",
			in:   "[git: neutron status",
			want: nil,
		},
		{
			name: "empty",
			in:   "[git: ]",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseGitDirectives(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("ParseGitDirectives(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// The allowlist is the same one [send to ...] uses. A supervisor's reply is
// shaped by agent output, which is shaped by whatever those agents read, so a
// project name appearing in it is a name and not authorisation.
func TestGitDirectiveRefusesAProjectOutsideTheFleet(t *testing.T) {
	in, _ := gitFixture(t)
	items := in.answerGitDirectives("supervisor", "[git: repo status]", map[string]bool{})
	if len(items) != 0 {
		t.Fatalf("ran a query against a project outside the fleet: %+v", items)
	}
	if !strings.Contains(kingThread(t, in), "not in this turn's fleet") {
		t.Error("the refusal was not recorded in the supervisor's thread")
	}
}

// The subcommand set is closed, so nothing assembled from model text reaches
// git.
func TestGitDirectiveRefusesAnUnknownKind(t *testing.T) {
	in, _ := gitFixture(t)
	allow := map[string]bool{"repo": true}
	for _, bad := range []string{"push", "status --exec=evil", "checkout"} {
		items := in.answerGitDirectives("supervisor", "[git: repo "+bad+"]", allow)
		if len(items) != 0 {
			t.Errorf("kind %q was accepted: %+v", bad, items)
		}
	}
}

func TestGitDirectiveAnswersLocally(t *testing.T) {
	in, _ := gitFixture(t)
	items := in.answerGitDirectives("supervisor", "[git: repo log]", map[string]bool{"repo": true})
	if len(items) != 1 {
		t.Fatalf("got %d answers, want 1", len(items))
	}
	// No agent turn: the answer is already here, so nothing waits on a handle.
	if items[0].handle != nil {
		t.Error("a git query was given a turn handle; it should be answered locally")
	}
	if !strings.Contains(items[0].answer, "first") {
		t.Errorf("answer does not contain the commit:\n%s", items[0].answer)
	}
	if !strings.Contains(items[0].name, "git log") {
		t.Errorf("name = %q, want it to say which query it was", items[0].name)
	}
}

// A clean tree and an empty diff are answers. Returning nothing would read as
// a failed query, and the supervisor would go and spend a turn asking.
func TestGitDirectiveReportsAnEmptyAnswerAsOne(t *testing.T) {
	in, _ := gitFixture(t)
	items := in.answerGitDirectives("supervisor", "[git: repo diff]", map[string]bool{"repo": true})
	if len(items) != 1 {
		t.Fatalf("got %d answers, want 1", len(items))
	}
	if !strings.Contains(items[0].answer, "nothing to report") {
		t.Errorf("empty diff rendered as %q", items[0].answer)
	}
}

// A project that is not a repository has to say so rather than looking like a
// broken query.
func TestGitDirectiveOnANonRepo(t *testing.T) {
	in, _ := gitFixture(t)
	items := in.answerGitDirectives("supervisor", "[git: plain status]", map[string]bool{"plain": true})
	if len(items) != 1 {
		t.Fatalf("got %d answers, want 1", len(items))
	}
	if !strings.Contains(items[0].answer, "failed") {
		t.Errorf("answer = %q, want it to report the failure", items[0].answer)
	}
}

// Locally-answered items pass through collectReplies in dispatch order,
// alongside anything waiting on a turn.
func TestCollectRepliesPassesLocalAnswersThrough(t *testing.T) {
	in, _ := gitFixture(t)
	got := in.collectReplies([]pending{
		{name: "a", answer: "first answer"},
		{name: "b", answer: "second answer"},
	}, kingRoundTimeout)
	if len(got) != 2 || got[0].content != "first answer" || got[1].content != "second answer" {
		t.Errorf("collectReplies = %+v", got)
	}
}

// The supervisor is told the cheap path exists. A model that does not know
// will spend an agent turn on a question a subprocess answers.
func TestKingContextTeachesTheGitDirective(t *testing.T) {
	in, _ := gitFixture(t)
	ctx := in.formatKingState([]string{"repo", "plain"})
	for _, want := range []string{"[git: ", "status", "diff", "log", "no tokens are spent"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("injected context is missing %q:\n%s", want, ctx)
		}
	}
}

// Tree state rides along in the fleet listing, which costs nothing and saves a
// round-trip.
func TestKingContextCarriesTreeState(t *testing.T) {
	in, _ := gitFixture(t)
	in.RefreshGit()
	ctx := in.formatKingState([]string{"repo"})
	if !strings.Contains(ctx, "{main}") {
		t.Errorf("branch not injected:\n%s", ctx)
	}
}

func TestGitKindsAreTheDocumentedThree(t *testing.T) {
	want := []git.Kind{git.KindStatus, git.KindDiff, git.KindLog}
	if !slices.Equal(git.Kinds, want) {
		t.Errorf("git.Kinds = %v, want %v", git.Kinds, want)
	}
}

func kingThread(t *testing.T, in *Inbox) string {
	t.Helper()
	for _, p := range in.Snapshot() {
		if p.Name != "supervisor" {
			continue
		}
		var b strings.Builder
		for _, m := range p.History {
			b.WriteString(m.Content + "\n")
		}
		return b.String()
	}
	return ""
}
