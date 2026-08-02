package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
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

	// ForkFrom names a session belonging to something else — the live agent
	// this project was adopted from. The first send seeds a new session with
	// its history rather than resuming it, and clears this once it has a
	// session of its own. Persisted, because an adoption that is never sent
	// to before a restart should still inherit its context afterwards.
	ForkFrom string `json:"fork_from,omitempty"`

	// Activity carries the current live status label while Status == Working:
	// e.g. "typing", "Bash", "Edit". Transient — not persisted; reset on
	// restart. Populated only when the driver implements StreamingDriver.
	Activity string `json:"-"`

	// StreamingText holds the in-progress assistant text during a streaming
	// turn. Updated on each StreamText event; cleared on StreamDone (the
	// final text goes to LastMessage + History). Transient — not persisted.
	// The TUI shows this in the detail view so the user can watch the
	// response arrive in real time instead of staring at a blank screen.
	StreamingText string `json:"-"`
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
	notesPath  string // empty = notes live for this session only

	// pollEvery overrides how often a king round checks its targets. Zero
	// means the default; set before any turn starts, never during one.
	pollEvery time.Duration

	// rounds is how many dispatch rounds one king turn may spend. Zero means
	// the default. Read under mu — a watcher reads it minutes after the turn
	// that started it.
	rounds int

	// notes are the supervisor's durable facts about the fleet.
	notes []Note

	// cancels maps project Name -> the cancel function for its in-flight
	// send goroutine. Empty when no send is active for that project.
	cancels map[string]context.CancelFunc

	// done is closed by Close. Watchers poll for minutes; without a stop
	// signal they outlive whatever started them and go on writing state
	// after the thing that owns it has gone.
	done      chan struct{}
	closeOnce sync.Once
	// wg tracks every background goroutine so Close can wait for them.
	// Signalling alone is not enough: a send goroutine already past the
	// stop check still has a write to make.
	wg sync.WaitGroup
}

func New(projects []*Project, drivers map[string]driver.Driver, statePath string) *Inbox {
	return &Inbox{
		projects:  projects,
		drivers:   drivers,
		statePath: statePath,
		cancels:   make(map[string]context.CancelFunc),
		done:      make(chan struct{}),
	}
}

// Close stops the background watchers and waits for them, so nothing writes
// state after it returns. In-flight sends are cancelled rather than waited
// out — a turn can take five minutes, and quitting should not.
//
// Safe to call more than once, and safe to call during a round.
func (in *Inbox) Close() {
	in.closeOnce.Do(func() {
		close(in.done)
		in.mu.Lock()
		for name, cancel := range in.cancels {
			delete(in.cancels, name)
			cancel()
		}
		in.mu.Unlock()
	})
	in.wg.Wait()
}

// track runs fn in a goroutine Close will wait for.
func (in *Inbox) track(fn func()) {
	in.wg.Add(1)
	go func() {
		defer in.wg.Done()
		fn()
	}()
}

// closed reports whether Close has been called.
func (in *Inbox) closed() bool {
	select {
	case <-in.done:
		return true
	default:
		return false
	}
}

// pause waits out one poll interval, reporting false if the inbox closed
// first so a watcher stops instead of finishing a round nobody will read.
func (in *Inbox) pause() bool {
	t := time.NewTimer(in.pollInterval())
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-in.done:
		return false
	}
}

// WithKingRounds sets how many dispatch rounds a single king turn may spend
// before it has to report back and wait for the user. Zero keeps the default
// of one; values above maxKingRounds are clamped.
func (in *Inbox) WithKingRounds(n int) *Inbox {
	in.mu.Lock()
	in.rounds = n
	in.mu.Unlock()
	return in
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
// Returns nil on success (including when config persistence fails — the
// project is added in-memory for the current session either way). A
// config-write failure is logged but not returned as an error, so the
// TUI's new-project modal doesn't wedge on a recoverable error.
//
// Returns an error only for validation failures: duplicate name/dir,
// or unknown tool.
func (in *Inbox) AddProject(name, tool, dir string) error {
	return in.addProject(name, tool, dir, "", "")
}

// AdoptProject registers a project already backed by an existing agent
// session, so its first send continues that conversation instead of starting
// blank. Both ids are persisted in state.json, not config.json — config
// describes which projects exist, state describes where they are.
//
// Exactly one of the two is normally set. sessionID is a session this project
// owns and can resume. forkFrom is somebody else's — a live agent's — and the
// first send forks it instead, which works while that agent runs and leaves it
// alone. Pass sessionID only when the driver can genuinely resume it.
func (in *Inbox) AdoptProject(name, tool, dir, sessionID, forkFrom string) error {
	return in.addProject(name, tool, dir, sessionID, forkFrom)
}

func (in *Inbox) addProject(name, tool, dir, sessionID, forkFrom string) error {
	in.mu.Lock()
	for _, p := range in.projects {
		if p.Name == name {
			in.mu.Unlock()
			return fmt.Errorf("a project named %q already exists (dir: %s)", name, p.Dir)
		}
		if p.Dir == dir {
			in.mu.Unlock()
			return fmt.Errorf("directory %q is already used by project %q", dir, p.Name)
		}
	}
	if _, ok := in.drivers[tool]; !ok {
		in.mu.Unlock()
		return fmt.Errorf("unknown tool %q (no driver registered)", tool)
	}
	in.projects = append(in.projects, &Project{
		Name:      name,
		Tool:      tool,
		Dir:       dir,
		SessionID: sessionID,
		ForkFrom:  forkFrom,
		Status:    driver.StatusIdle,
	})
	in.mu.Unlock()

	// Persist to config.json best-effort. Non-fatal — session still works.
	if in.configPath != "" {
		if err := in.appendConfig(name, tool, dir); err != nil {
			// Log but don't return error — the project IS added in-memory.
			// The TUI will show a toast; the user knows it won't persist.
			fmt.Fprintf(os.Stderr, "agent-inbox: warning: config persist failed: %v\n", err)
		}
	}
	in.save()
	return nil
}

func (in *Inbox) appendConfig(name, tool, dir string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot safely write config: load failed: %w", err)
		}
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

// projectByName is the stable lookup. Names are unique (addProject enforces
// it) and never move; indices shift on every removal. Callers must hold mu.
func (in *Inbox) projectByName(name string) (*Project, error) {
	for _, p := range in.projects {
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no project named %q", name)
}

// Send dispatches a prompt to project idx (1-based) in the background.
// The prompt is stored verbatim in history AND sent to the driver.
func (in *Inbox) Send(idx int, prompt string) error {
	return in.sendRaw(idx, prompt, prompt)
}

// sendRaw is the internal send implementation. displayText is what appears
// in history; driverText is what's sent to the CLI. For normal sends they're
// identical. For king sends, displayText is the user's original message and
// driverText includes the injected fleet state context.
func (in *Inbox) sendRaw(idx int, displayText, driverText string) error {
	return in.sendInternal(idx, displayText, driverText, true)
}

// sendInternal is sendRaw with control over whether the prompt is recorded as
// a user turn. The king's summary pass sends a prompt the user never typed;
// showing it as their message would be a lie about who said what.
func (in *Inbox) sendInternal(idx int, displayText, driverText string, record bool) error {
	return in.sendResolved(func() (*Project, error) { return in.project(idx) }, displayText, driverText, record)
}

// sendNamed sends to a project by name. Anything holding a reference across
// time must use this: an index is only valid until the next RemoveProject,
// which splices the slice and shifts every index after it down by one.
func (in *Inbox) sendNamed(name, displayText, driverText string, record bool) error {
	return in.sendResolved(func() (*Project, error) { return in.projectByName(name) }, displayText, driverText, record)
}

// sendResolved resolves the target under the same lock that starts the turn,
// so a project cannot be removed between the lookup and the send.
func (in *Inbox) sendResolved(resolve func() (*Project, error), displayText, driverText string, record bool) error {
	in.mu.Lock()
	p, err := resolve()
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
	// Append the DISPLAY text (user's original message) to history —
	// NOT the driverText which may include injected state context.
	if record {
		p.appendHistory(Message{Role: "user", Content: displayText, Timestamp: time.Now()})
	}
	dir, sid, forkFrom := p.Dir, p.SessionID, p.ForkFrom
	// Cancellable context with a 5-minute timeout so a stuck CLI
	// (rate-limited API, hung model) doesn't hang the project forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	in.cancels[p.Name] = cancel
	in.mu.Unlock()
	in.save()

	in.track(func() {
		// Send the DRIVER text (may include injected state) to the CLI.
		fd, canFork := d.(driver.ForkingDriver)
		sd, canStream := d.(driver.StreamingDriver)
		switch {
		case forkFrom != "" && canFork:
			// The fork turn does not stream. It is one turn, it happens once
			// per adoption, and giving it a second code path through the
			// streaming interface would double the surface for a spinner.
			in.blockingSend(p, func() driver.Result {
				return fd.SendForked(ctx, dir, forkFrom, driverText)
			})
		case forkFrom != "":
			// Nothing to fork with: start clean rather than resuming an id
			// this driver has no claim to.
			in.blockingSend(p, func() driver.Result { return d.Send(ctx, dir, "", driverText) })
		case canStream:
			in.streamSend(ctx, sd, p, dir, sid, driverText)
		default:
			in.blockingSend(p, func() driver.Result { return d.Send(ctx, dir, sid, driverText) })
		}
		in.mu.Lock()
		delete(in.cancels, p.Name)
		in.mu.Unlock()
		// Persist the finished turn. streamSend saves as events arrive, but
		// blockingSend — every non-streaming driver, which is opencode, codex
		// and mock — only mutated memory. Its reply reached the screen and
		// then vanished on the next restart.
		in.save()
	})
	return nil
}

// blockingSend runs a non-streaming turn and files its outcome. run is what
// actually talks to the CLI — an ordinary send, or a fork of somebody else's
// session — so the three ways to start a turn share one way to end it.
func (in *Inbox) blockingSend(p *Project, run func() driver.Result) {
	res := run()
	in.mu.Lock()
	defer in.mu.Unlock()
	// If the project was cancelled, the underlying subprocess was killed
	// and the driver returned with a killed-process error. We've already set
	// the status to Idle in Cancel(); skip the overwrite.
	if p.Status != driver.StatusWorking {
		return
	}
	if res.SessionID != "" {
		p.SessionID = res.SessionID
	}
	// A turn that got through has given this project a session of its own,
	// so the borrowed one it was seeded from is spent. A turn that failed
	// keeps it: the next attempt should still inherit the context.
	if res.Err == nil {
		p.ForkFrom = ""
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
			p.StreamingText = ""
		case driver.StreamText:
			p.Status = driver.StatusWorking
			p.Activity = "typing"
			p.StreamingText += ev.Content
			// Skip save — StreamingText and Activity are transient (json:"-").
			// The TUI reads from in-memory state via Snapshot(), not from disk.
			p.UpdatedAt = time.Now()
			in.mu.Unlock()
			continue
		case driver.StreamToolCall:
			p.Status = driver.StatusWorking
			p.Activity = ev.Activity
			// Skip save — Activity is transient.
			p.UpdatedAt = time.Now()
			in.mu.Unlock()
			continue
		case driver.StreamDone:
			p.Status = driver.StatusWaiting
			p.Activity = ""
			p.StreamingText = ""
			p.LastErr = ""
			p.LastMessage = ev.Content
			p.appendHistory(Message{Role: "assistant", Content: ev.Content, Timestamp: time.Now()})
		case driver.StreamError:
			p.Status = driver.StatusError
			p.Activity = ""
			msg := "turn failed"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			p.LastErr = msg
			// Preserve partial streaming text or error content.
			if p.StreamingText != "" {
				p.LastMessage = p.StreamingText
				p.appendHistory(Message{Role: "assistant", Content: p.StreamingText + "\n\n(error: " + msg + ")", Timestamp: time.Now()})
			} else if ev.Content != "" {
				p.LastMessage = ev.Content
				p.appendHistory(Message{Role: "error", Content: ev.Content + "\n\n(error: " + msg + ")", Timestamp: time.Now()})
			} else {
				p.appendHistory(Message{Role: "error", Content: msg, Timestamp: time.Now()})
			}
			p.StreamingText = ""
		}
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()
	}

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

// Cancel handles the user's "I'm done with this state" intent, with
// behavior that depends on what the project is currently doing:
//
//   - Working: kills the in-flight subprocess via the stored cancel func,
//     sets status to Idle, logs "cancelled by user" to history so the
//     gap between user prompt and (no) assistant reply is explained.
//
//   - Waiting or Error: no subprocess to kill — the agent already
//     finished. Just resets status to Idle. This is the "dismiss
//     notification" path: the user has seen the output and wants the
//     indicator cleared. No history entry (the conversation is
//     preserved as-is in the detail view).
//
//   - Idle: returns an error so the TUI can surface "already idle".
func (in *Inbox) Cancel(idx int) error {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return err
	}

	switch p.Status {
	case driver.StatusWorking:
		// Kill the in-flight subprocess.
		cancel, ok := in.cancels[p.Name]
		if !ok {
			// Defensive: status says working but no cancel func. Treat as
			// a stuck state and reset to idle without killing anything.
			p.Status = driver.StatusIdle
			p.Activity = ""
			p.LastErr = "stuck (no cancel func); reset to idle"
			in.mu.Unlock()
			in.save()
			return nil
		}
		delete(in.cancels, p.Name)
		p.Status = driver.StatusIdle
		p.Activity = ""
		p.LastErr = "cancelled by user"
		p.appendHistory(Message{Role: "system", Content: "cancelled by user", Timestamp: time.Now()})
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()

		cancel() // signal the subprocess to die; goroutine will no-op on return
		return nil

	case driver.StatusWaiting, driver.StatusError:
		// Dismiss the notification — agent already finished, just reset status.
		previous := p.Status
		p.Status = driver.StatusIdle
		p.Activity = ""
		// Don't clear LastErr/LastMessage — the detail view should still
		// show what happened. Just the "waiting" / "error" indicator clears.
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()
		_ = previous
		return nil

	default:
		// Already idle.
		in.mu.Unlock()
		return fmt.Errorf("%s is already idle", p.Name)
	}
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
	in.forgetProject(name)

	// Persist removal to config.json best-effort (non-fatal — matches AddProject).
	if in.configPath != "" {
		if err := in.removeProjectConfig(name); err != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: warning: config persist failed: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "agent-inbox: warning: config persist failed: %v\n", err)
		}
	}
	return nil
}

func (in *Inbox) removeProjectConfig(name string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot safely write config: load failed: %w", err)
		}
		settings = &config.Settings{}
	}
	settings.RemoveProject(name)
	return config.Save(in.configPath, settings)
}

func (in *Inbox) setProjectToolConfig(name, tool string) error {
	settings, err := config.Load(in.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot safely write config: load failed: %w", err)
		}
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

// save writes state.json atomically (temp file + rename) so a crash
// during write can't corrupt the existing state file.
func (in *Inbox) save() {
	if in.closed() {
		return
	}
	in.mu.Lock()
	b, err := json.MarshalIndent(in.projects, "", "  ")
	in.mu.Unlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(in.statePath)
	_ = os.MkdirAll(dir, 0o755)
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), in.statePath); err != nil {
		os.Remove(tmp.Name())
	}
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
