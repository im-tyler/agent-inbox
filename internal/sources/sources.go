package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/im-tyler/agent-inbox/internal/feed"
)

// Source is anything that can produce a teploy.inbox/v1 feed. The consumer
// knows nothing else about it.
type Source interface {
	Name() string
	Fetch(ctx context.Context) (feed.Feed, error)
}

// Exec runs a command that prints the envelope on stdout — the CLI form of the
// contract, e.g. `teploy-ship inbox --json`.
type Exec struct {
	Label   string
	Command []string
	Dir     string
	Timeout time.Duration
}

func (e Exec) Name() string { return e.Label }

func (e Exec) Fetch(ctx context.Context) (feed.Feed, error) {
	if len(e.Command) == 0 {
		return feed.Feed{}, fmt.Errorf("%s: no command configured", e.Label)
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.Command[0], e.Command[1:]...)
	cmd.Dir = e.Dir
	// Bounded writers. A producer stuck in a loop printing to stdout would
	// otherwise be copied into memory until the process died.
	stdout := &boundedBuffer{limit: maxSourceBytes}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// Producers write human output to stderr, so it usually explains
		// the failure better than the exit status does.
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return feed.Feed{}, fmt.Errorf("%s: %v: %s", e.Label, err, truncate(detail, 200))
		}
		return feed.Feed{}, fmt.Errorf("%s: %w", e.Label, err)
	}
	if stdout.overflowed {
		return feed.Feed{}, fmt.Errorf("%s: output exceeded %d bytes", e.Label, maxSourceBytes)
	}
	return decode(e.Label, stdout.Bytes())
}

const (
	// maxSourceBytes bounds one source's feed. Large enough for thousands of
	// items, small enough that a broken producer cannot exhaust memory.
	maxSourceBytes = 8 << 20
	// maxStderrBytes bounds the diagnostic text kept from a failing producer.
	maxStderrBytes = 64 << 10
	// maxItemsPerSource bounds how many rows one source may contribute.
	maxItemsPerSource = 5000
)

// boundedBuffer accumulates up to limit bytes and then discards the rest,
// recording that it did.
type boundedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflowed bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.overflowed = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.overflowed = true
	}
	// Report the full length: a short write makes the child see a broken pipe,
	// which is a different failure from the one we want to report.
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *boundedBuffer) String() string { return b.buf.String() }

// HTTP fetches GET <URL>, the server form of the contract.
type HTTP struct {
	Label string
	URL   string
	// TokenEnv names an environment variable holding a bearer token. The
	// token itself is never stored in config.
	TokenEnv string
	Timeout  time.Duration
	Client   *http.Client
}

func (h HTTP) Name() string { return h.Label }

func (h HTTP) Fetch(ctx context.Context) (feed.Feed, error) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the URL through net/url rather than by string suffix. Appending
	// "/inbox" to a URL carrying a query string produced ".../?x=1/inbox".
	endpoint, err := inboxURL(h.URL)
	if err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	token := ""
	if h.TokenEnv != "" {
		token = os.Getenv(h.TokenEnv)
	}
	// A bearer token on a plaintext connection is a credential handed to
	// anyone on the path. Loopback is exempt: there is no path.
	if token != "" && endpoint.Scheme != "https" && !isLoopback(endpoint.Hostname()) {
		return feed.Feed{}, fmt.Errorf("%s: refusing to send a bearer token over %s to %s — use https",
			h.Label, endpoint.Scheme, endpoint.Host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return feed.Feed{}, fmt.Errorf("%s: http %d", h.Label, resp.StatusCode)
	}
	// Read one byte past the limit so an oversized body is detected rather
	// than silently truncated into a parse error.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBytes+1))
	if err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	if len(body) > maxSourceBytes {
		return feed.Feed{}, fmt.Errorf("%s: response exceeded %d bytes", h.Label, maxSourceBytes)
	}
	return decode(h.Label, body)
}

// inboxURL resolves a configured base URL to its /inbox endpoint.
func inboxURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("not an absolute url: %q", raw)
	}
	if !strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "/inbox") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/inbox"
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// decode tolerates a bare array as well as the envelope. Producers get this
// wrong at first and a readable list beats a strict parser.
//
// An explicit schema, though, is checked. Lenience about missing fields is
// forwards compatibility; accepting a schema we do not implement is agreeing
// to a contract we have not read, and this data reaches an action executor.
func decode(label string, data []byte) (feed.Feed, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return feed.Feed{Schema: feed.Schema}, nil
	}
	var f feed.Feed
	if trimmed[0] == '[' {
		var items []feed.Item
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return feed.Feed{}, fmt.Errorf("%s: %w", label, err)
		}
		f = feed.Feed{Schema: feed.Schema, Items: items}
	} else {
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return feed.Feed{}, fmt.Errorf("%s: %w", label, err)
		}
	}
	if f.Schema != "" && f.Schema != feed.Schema {
		return feed.Feed{}, fmt.Errorf("%s: unsupported schema %q (this build speaks %s)", label, f.Schema, feed.Schema)
	}
	if len(f.Items) > maxItemsPerSource {
		f.Items = f.Items[:maxItemsPerSource]
		f.Truncated = true
	}
	return f, nil
}

// Result pairs a source's feed with whatever went wrong reaching it. A dead
// source must never blank the whole list — it reports itself and the rest of
// the inbox still renders.
type Result struct {
	Source string
	Feed   feed.Feed
	Err    error
	// Warnings are per-item problems that did not stop the source producing a
	// usable feed: an item with no id, an unsupported schema on one row.
	Warnings []error
}

// Aggregate is the outcome of querying every source.
//
// Truncated is the part that matters for a machine reading this. `agent-inbox
// inbox --json` is documented for scripts and agents, and it emitted a valid
// envelope whether or not a source had failed: a consumer could read "no items"
// and conclude nothing was waiting, when in fact the one source holding a
// pending decision was unreachable. False completeness is worse than an error,
// because only one of the two gets noticed.
type Aggregate struct {
	Items     []feed.Item
	Results   []Result
	Truncated bool
}

// FetchAll queries every source concurrently and returns the merged, sorted
// items along with per-source outcomes.
func FetchAll(ctx context.Context, srcs []Source) ([]feed.Item, []Result) {
	agg := Fetch(ctx, srcs)
	return agg.Items, agg.Results
}

// Fetch queries every source concurrently and reports whether the merged
// result is complete.
func Fetch(ctx context.Context, srcs []Source) Aggregate {
	results := make([]Result, len(srcs))
	var wg sync.WaitGroup
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			f, err := src.Fetch(ctx)
			results[i] = Result{Source: src.Name(), Feed: f, Err: err}
		}(i, src)
	}
	wg.Wait()

	agg := Aggregate{Results: results}
	feeds := make([]feed.Feed, 0, len(results))
	for ri := range results {
		r := &results[ri]
		if r.Err != nil {
			agg.Truncated = true // a source we could not reach may hold anything
			continue
		}
		if r.Feed.Truncated {
			agg.Truncated = true
		}
		kept := r.Feed.Items[:0]
		for i := range r.Feed.Items {
			r.Feed.Items[i].Normalize(r.Source)
			// An item without an id shares a Key with every other id-less item
			// from the same source, so Merge would silently collapse them into
			// one row. Dropping it with a warning at least says so.
			if err := r.Feed.Items[i].Validate(); err != nil {
				r.Warnings = append(r.Warnings, fmt.Errorf("%s: %w", r.Source, err))
				agg.Truncated = true
				continue
			}
			kept = append(kept, r.Feed.Items[i])
		}
		r.Feed.Items = kept
		feeds = append(feeds, r.Feed)
	}
	agg.Items = feed.Merge(feeds)
	return agg
}

// Errors returns the failing sources in a stable order, for a status line.
func Errors(results []Result) []Result {
	var bad []Result
	for _, r := range results {
		if r.Err != nil {
			bad = append(bad, r)
		}
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].Source < bad[j].Source })
	return bad
}
