package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"agentinbox/internal/config"
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

	// Activity carries the current live status label while Status == Working:
	// e.g. "typing", "Bash", "Edit". Transient — not persisted; reset on
	// restart. Populated only when the driver implements StreamingDriver.
	Activity string `json:"-"`
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
	mu         sync.Mutex
	projects   []*Project
	drivers    map[string]driver.Driver
	statePath  string
	configPath string // empty = AddProject can't persist to config

	// cancels maps project Name -> the cancel function for its in-flight
	// send goroutine. Empty when no send is active for that project.
	cancels map[string]context.CancelFunc
}

func New(projects []*Project, drivers map[string]driver.Driver, statePath string) *Inbox {
	return &Inbox{
		projects: projects,
		drivers:  drivers,
		statePath: statePath,
		cancels:  make(map[string]context.CancelFunc),
	}
}

// WithConfigPath enables runtime project addition via AddProject; the path
// is rewritten on each AddProject call so new projects persist alongside
// the original configuration.
func (in *Inbox) WithConfigPath(p string) *Inbox {
	in.configPath = p
	return in
}

// AddProject appends a new project in-memory, attempts to persist it to
// config.json (so it survives restart), and saves state.json.
//
// Returns an error if the project is a duplicate (same name or dir) or if
// persisting to config fails. A config-path error is non-fatal — the project
// is still added in-memory and to state.json so the current session works.
func (in *Inbox) AddProject(name, tool, dir string) error {
	in.mu.Lock()
	for _, p := range in.projects {
		if p.Name == name || p.Dir == dir {
			in.mu.Unlock()
			return fmt.Errorf("duplicate project (name=%q or dir=%q already exists)", name, dir)
		}
	}
	if _, ok := in.drivers[tool]; !ok {
		in.mu.Unlock()
		return fmt.Errorf("unknown tool %q (no driver registered)", tool)
	}
	in.projects = append(in.projects, &Project{
		Name:   name,
		Tool:   tool,
		Dir:    dir,
		Status: driver.StatusIdle,
	})
	in.mu.Unlock()

	// Persist to config.json best-effort.
	if in.configPath != "" {
		if err := in.appendConfig(name, tool, dir); err != nil {
			// Non-fatal — session still works; just won't survive restart.
			return fmt.Errorf("added in-memory but failed to persist config: %w", err)
		}
	}
	in.save()
	return nil
}

func (in *Inbox) appendConfig(name, tool, dir string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		// Config might have been hand-edited to allow zero projects, or
		// we're writing into a fresh file. Either way, start fresh.
		settings = &config.Settings{}
	}
	settings.AddProject(config.Project{Name: name, Tool: tool, Dir: dir})
	return config.Save(in.configPath, settings)
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
	// Cancellable context so Cancel() can kill the underlying subprocess.
	ctx, cancel := context.WithCancel(context.Background())
	in.cancels[p.Name] = cancel
	in.mu.Unlock()
	in.save()

	go func() {
		// If the driver streams, use the streaming path so the UI can show
		// live activity (tool name, typing). Otherwise fall back to blocking Send.
		if sd, ok := d.(driver.StreamingDriver); ok {
			in.streamSend(ctx, sd, p, dir, sid, prompt)
		} else {
			in.blockingSend(ctx, d, p, dir, sid, prompt)
		}
		// Clear the cancel func regardless of path.
		in.mu.Lock()
		delete(in.cancels, p.Name)
		in.mu.Unlock()
	}()
	return nil
}

// blockingSend is the non-streaming send path. Extracted from Send so the
// streaming path can share the same cleanup logic.
func (in *Inbox) blockingSend(ctx context.Context, d driver.Driver, p *Project, dir, sid, prompt string) {
	res := d.Send(ctx, dir, sid, prompt)
	in.mu.Lock()
	defer in.mu.Unlock()
	// If the project was cancelled, the underlying subprocess was killed
	// and d.Send returned with a killed-process error. We've already set
	// the status to Idle in Cancel(); skip the overwrite.
	if p.Status != driver.StatusWorking {
		return
	}
	if res.SessionID != "" {
		p.SessionID = res.SessionID
	}
	p.Status = res.Status
	p.Activity = ""
	if res.Err != nil {
		p.LastErr = res.Err.Error()
		p.appendHistory(Message{Role: "error", Content: res.Err.Error(), Timestamp: time.Now()})
	} else {
		p.LastErr = ""
		p.LastMessage = res.Final
		p.appendHistory(Message{Role: "assistant", Content: res.Final, Timestamp: time.Now()})
	}
	p.UpdatedAt = time.Now()
}

// streamSend consumes a StreamingDriver's event channel and updates the
// project's state live. Emits one final assistant (or error) history entry
// when the turn completes, identical to the blocking path's behavior.
//
// Must be called from a background goroutine (it is — by Send's caller).
// Holds the inbox mutex briefly per event to mutate Project state.
func (in *Inbox) streamSend(ctx context.Context, sd driver.StreamingDriver, p *Project, dir, sid, prompt string) {
	ch := sd.StreamSend(ctx, dir, sid, prompt)

	var finalText string
	var finalErr error

	for ev := range ch {
		in.mu.Lock()
		// If cancelled, stop processing events.
		if p.Status != driver.StatusWorking {
			in.mu.Unlock()
			return
		}
		if ev.SessionID != "" {
			p.SessionID = ev.SessionID
		}
		switch ev.Kind {
		case driver.StreamStarted:
			p.Status = driver.StatusWorking
			p.Activity = ev.Activity
			if p.Activity == "" {
				p.Activity = "starting"
			}
		case driver.StreamText:
			p.Status = driver.StatusWorking
			p.Activity = "typing"
			finalText += ev.Content
		case driver.StreamToolCall:
			p.Status = driver.StatusWorking
			p.Activity = ev.Activity
		case driver.StreamDone:
			finalText = ev.Content
			p.Status = driver.StatusWaiting
			p.Activity = ""
			p.LastErr = ""
			p.LastMessage = finalText
			p.appendHistory(Message{Role: "assistant", Content: finalText, Timestamp: time.Now()})
		case driver.StreamError:
			finalErr = ev.Err
			p.Status = driver.StatusError
			p.Activity = ""
			msg := "turn failed"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			p.LastErr = msg
			p.appendHistory(Message{Role: "error", Content: msg, Timestamp: time.Now()})
		}
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()
	}

	_ = finalErr // already surfaced via StreamError if non-nil

	// If the channel closed without a Done or Error event, treat as error
	// (unless we were cancelled, which is handled above).
	in.mu.Lock()
	defer in.mu.Unlock()
	if p.Status == driver.StatusWorking {
		p.Status = driver.StatusError
		p.Activity = ""
		p.LastErr = "stream ended without completion event"
	}
}

// Cancel kills the in-flight send for project idx (1-based). Returns an
// error if the project isn't currently working.
//
// Cancellation is cooperative: the underlying subprocess receives a
// SIGTERM (via exec.CommandContext) and the goroutine exits. Status is
// immediately set to Idle so the UI reflects the cancellation before the
// subprocess has fully terminated.
func (in *Inbox) Cancel(idx int) error {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	cancel, ok := in.cancels[p.Name]
	if !ok {
		in.mu.Unlock()
		return fmt.Errorf("%s is not currently working", p.Name)
	}
	delete(in.cancels, p.Name)
	p.Status = driver.StatusIdle
	p.Activity = ""
	p.LastErr = "cancelled by user"
	p.appendHistory(Message{Role: "error", Content: "cancelled by user", Timestamp: time.Now()})
	p.UpdatedAt = time.Now()
	in.mu.Unlock()
	in.save()

	cancel() // signal the subprocess to die; goroutine will no-op on return
	return nil
}

// RemoveProject deletes project idx (1-based) from the in-memory list,
// state.json, and config.json. If the project has an in-flight send, it
// is cancelled first.
func (in *Inbox) RemoveProject(idx int) error {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	name := p.Name
	// Cancel any in-flight send before removing.
	if cancel, ok := in.cancels[name]; ok {
		delete(in.cancels, name)
		cancel()
	}
	// Remove from slice (preserves order).
	in.projects = append(in.projects[:idx-1], in.projects[idx:]...)
	in.mu.Unlock()
	in.save()

	// Persist removal to config.json best-effort.
	if in.configPath != "" {
		if err := in.removeProjectConfig(name); err != nil {
			return fmt.Errorf("removed in-memory but failed to persist config: %w", err)
		}
	}
	return nil
}

// SetProjectTool changes the driver for project idx (1-based). Clears the
// session id (a Claude session can't be resumed by OpenCode, etc.) and
// blocks if a send is currently in-flight.
func (in *Inbox) SetProjectTool(idx int, tool string) error {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	if _, ok := in.drivers[tool]; !ok {
		in.mu.Unlock()
		return fmt.Errorf("unknown tool %q", tool)
	}
	if _, working := in.cancels[p.Name]; working {
		in.mu.Unlock()
		return fmt.Errorf("%s is currently working — cancel before changing tool", p.Name)
	}
	p.Tool = tool
	p.SessionID = "" // previous session is meaningless to the new tool
	p.Status = driver.StatusIdle
	p.Activity = ""
	p.UpdatedAt = time.Now()
	name := p.Name
	in.mu.Unlock()
	in.save()

	if in.configPath != "" {
		if err := in.setProjectToolConfig(name, tool); err != nil {
			return fmt.Errorf("changed in-memory but failed to persist config: %w", err)
		}
	}
	return nil
}

func (in *Inbox) removeProjectConfig(name string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		settings = &config.Settings{}
	}
	settings.RemoveProject(name)
	return config.Save(in.configPath, settings)
}

func (in *Inbox) setProjectToolConfig(name, tool string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		settings = &config.Settings{}
	}
	settings.SetProjectTool(name, tool)
	return config.Save(in.configPath, settings)
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
