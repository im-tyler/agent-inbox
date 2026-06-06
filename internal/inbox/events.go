package inbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentinbox/internal/driver"
)

// Event is what a Stop hook drops on disk for the inbox to ingest. It lets
// sessions the inbox did not spawn (e.g. a Claude session you run by hand)
// report their state into the central inbox.
type Event struct {
	SessionID string `json:"session_id"`
	Dir       string `json:"dir"`
	Tool      string `json:"tool"`
	Message   string `json:"message"`
	TS        int64  `json:"ts"`
}

func WriteEvent(eventsDir string, ev Event) error {
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s.json", ev.TS, shortID(ev.SessionID))
	b, _ := json.MarshalIndent(ev, "", "  ")
	return os.WriteFile(filepath.Join(eventsDir, name), b, 0o644)
}

// Ingest applies and removes pending event files, returning the names of
// projects newly flipped to waiting.
func (in *Inbox) Ingest(eventsDir string) []string {
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		return nil
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // names are ts-prefixed, so oldest first
	var updated []string
	for _, fn := range files {
		full := filepath.Join(eventsDir, fn)
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var ev Event
		if json.Unmarshal(b, &ev) == nil {
			if name, ok := in.applyEvent(ev); ok {
				updated = append(updated, name)
			}
		}
		os.Remove(full)
	}
	if len(updated) > 0 {
		in.save()
	}
	return updated
}

func (in *Inbox) applyEvent(ev Event) (string, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, p := range in.projects {
		if !SameDir(p.Dir, ev.Dir) {
			continue
		}
		if ev.SessionID != "" {
			p.SessionID = ev.SessionID
		}
		p.Status = driver.StatusWaiting
		if ev.Message != "" {
			p.LastMessage = ev.Message
		}
		p.UpdatedAt = time.Unix(ev.TS, 0)
		return p.Name, true
	}
	return "", false
}

// SameDir compares two paths tolerant of symlinks (e.g. /tmp vs /private/tmp).
func SameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ra, ea := filepath.EvalSymlinks(ca)
	rb, eb := filepath.EvalSymlinks(cb)
	return ea == nil && eb == nil && ra == rb
}

func shortID(s string) string {
	s = strings.ReplaceAll(s, "/", "")
	if s == "" {
		return "noid"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
