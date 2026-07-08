package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentinbox/internal/driver"
)

type Project struct {
	Name        string        `json:"name"`
	Tool        string        `json:"tool"`
	Dir         string        `json:"dir"`
	SessionID   string        `json:"session_id"`
	Status      driver.Status `json:"status"`
	LastMessage string        `json:"last_message"`
	LastErr     string        `json:"last_err"`
	UpdatedAt   time.Time     `json:"updated_at"`
	History     []Message     `json:"history,omitempty"`
}

// Message is a single turn in a project's conversation history.
type Message struct {
	Role      string    `json:"role"` // "user", "assistant", or "error"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Inbox holds the federated set of project sessions and orchestrates sends.
// All state access is guarded by mu; sends run in background goroutines.
type Inbox struct {
	mu        sync.Mutex
	projects  []*Project
	drivers   map[string]driver.Driver
	statePath string
}

func New(projects []*Project, drivers map[string]driver.Driver, statePath string) *Inbox {
	return &Inbox{projects: projects, drivers: drivers, statePath: statePath}
}

// Snapshot returns a copy of the current project states for display.
func (in *Inbox) Snapshot() []Project {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]Project, len(in.projects))
	for i, p := range in.projects {
		out[i] = *p
	}
	return out
}

func (in *Inbox) WaitingCount() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for _, p := range in.projects {
		if p.Status == driver.StatusWaiting || p.Status == driver.StatusError {
			n++
		}
	}
	return n
}

func (in *Inbox) project(idx int) (*Project, error) {
	if idx < 1 || idx > len(in.projects) {
		return nil, fmt.Errorf("no project %d (have 1..%d)", idx, len(in.projects))
	}
	return in.projects[idx-1], nil
}

// Send dispatches a prompt to project idx (1-based) in the background.
func (in *Inbox) Send(idx int, prompt string) error {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	if p.Status == driver.StatusWorking {
		in.mu.Unlock()
		return fmt.Errorf("%s is already working", p.Name)
	}
	d, ok := in.drivers[p.Tool]
	if !ok {
		in.mu.Unlock()
		return fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	p.Status = driver.StatusWorking
	p.UpdatedAt = time.Now()
	// Append the user turn to history immediately so it's persisted even
	// if the agent crashes mid-turn.
	p.appendHistory(Message{Role: "user", Content: prompt, Timestamp: time.Now()})
	dir, sid := p.Dir, p.SessionID
	in.mu.Unlock()
	in.save()

	go func() {
		res := d.Send(context.Background(), dir, sid, prompt)
		in.mu.Lock()
		if res.SessionID != "" {
			p.SessionID = res.SessionID
		}
		p.Status = res.Status
		if res.Err != nil {
			p.LastErr = res.Err.Error()
			p.appendHistory(Message{Role: "error", Content: res.Err.Error(), Timestamp: time.Now()})
		} else {
			p.LastErr = ""
			p.LastMessage = res.Final
			p.appendHistory(Message{Role: "assistant", Content: res.Final, Timestamp: time.Now()})
		}
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()
	}()
	return nil
}

// appendHistory trims to the last 100 messages to bound state.json growth.
func (p *Project) appendHistory(m Message) {
	p.History = append(p.History, m)
	if len(p.History) > 100 {
		p.History = p.History[len(p.History)-100:]
	}
}

// Detail returns a project copy by 1-based index.
func (in *Inbox) Detail(idx int) (Project, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.project(idx)
	if err != nil {
		return Project{}, err
	}
	return *p, nil
}

// AttachArgs returns the interactive argv and working dir for project idx.
func (in *Inbox) AttachArgs(idx int) ([]string, string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.project(idx)
	if err != nil {
		return nil, "", err
	}
	if p.SessionID == "" {
		return nil, "", fmt.Errorf("%s has no session yet — send it a message first", p.Name)
	}
	d, ok := in.drivers[p.Tool]
	if !ok {
		return nil, "", fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	return d.AttachArgs(p.Dir, p.SessionID), p.Dir, nil
}

func (in *Inbox) save() {
	in.mu.Lock()
	b, err := json.MarshalIndent(in.projects, "", "  ")
	in.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(in.statePath), 0o755)
	_ = os.WriteFile(in.statePath, b, 0o644)
}

// LoadState overlays persisted session ids and last messages (matched by name)
// onto the project set defined by config.
func LoadState(path string, projects []*Project) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var saved []Project
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	byName := make(map[string]Project, len(saved))
	for _, s := range saved {
		byName[s.Name] = s
	}
	for _, p := range projects {
		s, ok := byName[p.Name]
		if !ok {
			continue
		}
		p.SessionID = s.SessionID
		p.Status = s.Status
		p.LastMessage = s.LastMessage
		p.LastErr = s.LastErr
		p.UpdatedAt = s.UpdatedAt
		p.History = s.History
		if p.Status == driver.StatusWorking {
			p.Status = driver.StatusIdle // a send can't survive a restart
		}
	}
}
