package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Producers write human output to stderr, so it usually explains
		// the failure better than the exit status does.
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return feed.Feed{}, fmt.Errorf("%s: %v: %s", e.Label, err, truncate(detail, 200))
		}
		return feed.Feed{}, fmt.Errorf("%s: %w", e.Label, err)
	}
	return decode(e.Label, stdout.Bytes())
}

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

	url := strings.TrimSuffix(h.URL, "/")
	if !strings.HasSuffix(url, "/inbox") {
		url += "/inbox"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	if h.TokenEnv != "" {
		if token := os.Getenv(h.TokenEnv); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", h.Label, err)
	}
	return decode(h.Label, body.Bytes())
}

// decode tolerates a bare array as well as the envelope. Producers get this
// wrong at first and a readable list beats a strict parser.
func decode(label string, data []byte) (feed.Feed, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return feed.Feed{Schema: feed.Schema}, nil
	}
	if trimmed[0] == '[' {
		var items []feed.Item
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return feed.Feed{}, fmt.Errorf("%s: %w", label, err)
		}
		return feed.Feed{Schema: feed.Schema, Items: items}, nil
	}
	var f feed.Feed
	if err := json.Unmarshal(trimmed, &f); err != nil {
		return feed.Feed{}, fmt.Errorf("%s: %w", label, err)
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
}

// FetchAll queries every source concurrently and returns the merged, sorted
// items along with per-source outcomes.
func FetchAll(ctx context.Context, srcs []Source) ([]feed.Item, []Result) {
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

	feeds := make([]feed.Feed, 0, len(results))
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		for i := range r.Feed.Items {
			r.Feed.Items[i].Normalize(r.Source)
		}
		feeds = append(feeds, r.Feed)
	}
	return feed.Merge(feeds), results
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
