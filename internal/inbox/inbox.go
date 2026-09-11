package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/im-tyler/agent-inbox/internal/claim"
	"github.com/im-tyler/agent-inbox/internal/config"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/git"
	"github.com/im-tyler/agent-inbox/internal/ident"
	"github.com/im-tyler/agent-inbox/internal/usage"
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

	// WaitReason is why this project is waiting, when a hook was specific
	// enough to say. Persisted, because a session blocked on a permission
	// prompt is still blocked after a restart, and coming back as an
	// undifferentiated "waiting" loses the one fact that decides what to do
	// about it. WaitDetail is the specific ask, when there is one.
	WaitReason Reason `json:"wait_reason,omitempty"`
	WaitDetail string `json:"wait_detail,omitempty"`

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

	// Git is the working tree as it was last read. Transient, like the two
	// above, and for a reason that is not just tidiness: the tree changes while
	// this program is not running, so a persisted branch restored at startup
	// would be presented as current when it describes whenever we last
	// happened to look.
	Git git.State `json:"-"`

	// Trace is what the current or most recent turn did: its tool calls, in
	// order. Unlike Activity it is persisted, because the question it answers
	// — what was this agent doing all that time — is asked most often by
	// somebody who was not watching while it happened: a harness that sent
	// the turn headless, or one that adopted the fleet mid-flight. Cleared
	// when the next turn starts, so it describes one turn, not a history.
	Trace []TraceEntry `json:"trace,omitempty"`

	// SeenEvents names the spool files already applied to this project,
	// newest last, bounded. Persisted with the rest of the entry so a crash
	// between the state commit and the spool removal cannot double-apply an
	// event — its history line, its supervision notice — on restart.
	SeenEvents []string `json:"seen_events,omitempty"`

	// Revision counts semantic changes to this project: completed turns and
	// applied hook events. Status alone cannot express a full round trip —
	// waiting → working → waiting looks like no change at all — and text
	// comparison cannot tell two identical replies apart (F21). Persisted,
	// monotonic, and compared by fleet follow as part of its fingerprint.
	Revision uint64 `json:"revision,omitempty"`
}

// bumpRevision records one semantic change. Caller holds mu.
func (p *Project) bumpRevision() {
	p.Revision++
}

// maxSeenEvents bounds the dedup set. Events arrive on a tick and are removed
// once committed; the set only has to cover a crash window, not history.
const maxSeenEvents = 32

// seenEvent records a spool file as applied. Caller holds mu (via
// applyEvent).
func (p *Project) seenEvent(name string) {
	for _, s := range p.SeenEvents {
		if s == name {
			return
		}
	}
	p.SeenEvents = append(p.SeenEvents, name)
	if len(p.SeenEvents) > maxSeenEvents {
		p.SeenEvents = p.SeenEvents[len(p.SeenEvents)-maxSeenEvents:]
	}
}

// hasSeenEvent reports whether a spool file was already applied.
func (p *Project) hasSeenEvent(name string) bool {
	for _, s := range p.SeenEvents {
		if s == name {
			return true
		}
	}
	return false
}

// TraceEntry is one thing the agent did during a turn.
type TraceEntry struct {
	T   time.Time `json:"t"`
	Act string    `json:"act"`
}

// maxTrace bounds one turn's trace. It is a progress indicator, not a
// transcript; a turn needing more entries than this reports the most recent
// ones, which is the half that says what it is doing now.
const maxTrace = 100

// appendTrace records one activity. Caller holds mu.
func (p *Project) appendTrace(act string) {
	if act == "" {
		return
	}
	p.Trace = append(p.Trace, TraceEntry{T: time.Now(), Act: act})
	if len(p.Trace) > maxTrace {
		p.Trace = p.Trace[len(p.Trace)-maxTrace:]
	}
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

	// groups partition the fleet: each one is a supervisor and the projects it
	// oversees. There is always at least one once WithGroups has run, and a
	// fleet with no configured groups is expressed as a single group holding
	// everything — so the code below has one shape to handle, not two.
	//
	// Membership is by name rather than index, because the supervisor has to
	// survive the list moving: adding and removing projects reorders indices,
	// and the king was once "whichever project happens to be first", which made
	// the supervisor an accident of config ordering.
	groups []Group

	// notes are the supervisor's durable facts about the fleet, plus any
	// standing rules it has proposed and the user has not ratified.
	notes []Note

	// rules are the ratified standing rules, read from config. The supervisor
	// cannot write here — see rules.go for why that separation exists.
	rules []Rule

	// cancels maps project Name -> the cancel function for its in-flight
	// send goroutine. Empty when no send is active for that project.
	cancels map[string]context.CancelFunc

	// turnTimeout bounds a single agent turn. Zero means no deadline.
	turnTimeout time.Duration

	// nextTurnID hands out turn identities. A turn is the unit watchers wait
	// on; see activeTurn.
	nextTurnID TurnID

	// active maps project name -> the turn currently running for it. A
	// completing turn only applies its result when it is still the active one,
	// so a subprocess that finishes after its turn was superseded cannot
	// overwrite the turn that replaced it.
	active map[string]*activeTurn

	// done is closed by Close. Watchers poll for minutes; without a stop
	// signal they outlive whatever started them and go on writing state
	// after the thing that owns it has gone.
	done      chan struct{}
	closeOnce sync.Once

	// bgCtx bounds background work that is not a turn — the git refresh, and
	// anything else that runs on a timer. Close cancels it, so quitting does
	// not wait out a subprocess's own timeout before the program can exit.
	bgCtx    context.Context
	bgCancel context.CancelFunc

	// gitNudge asks for an out-of-band tree refresh. Buffered at one: several
	// turns finishing together want the same single refresh, not one each.
	gitNudge chan struct{}

	// usageSrc reads how much has been spent against the rate limit; nil when
	// none is configured. usageSnap is its last good answer, kept across a
	// failed read so a transient error does not blank a figure that was right a
	// moment ago.
	usageSrc  usage.Source
	usageSnap usage.Snapshot
	usageErr  error

	// claims guards sends across processes: the Working check above only
	// sees this process's memory, and a harness-driven send in another one
	// is invisible to it. See multi.go and internal/claim.
	claims *claim.Set
	// lastStateMtime and lastNotesMtime are the mtimes of the files this
	// process last wrote, so RefreshExternal can tell its own writes from
	// everybody else's.
	lastStateMtime time.Time
	lastNotesMtime time.Time

	// overseer wakes a supervisor when its fleet changes, or nil when autonomy
	// is off — which is the default, because it is the one thing here that
	// spends money while nobody is watching.
	overseer *Overseer
	// draft is whether the user has an unsent message in the composer, told to
	// us by the UI. See SetDraft.
	draft bool
	// wg tracks every background goroutine so Close can wait for them.
	// Signalling alone is not enough: a send goroutine already past the
	// stop check still has a write to make.
	wg sync.WaitGroup

	// lifecycleMu serialises accepting new background work against shutdown.
	// wg.Add raced with wg.Wait without it: a watcher scheduling its next
	// round as Close ran could add to a counter Close had already observed at
	// zero, and the goroutine outlived the Close that promised to wait for it.
	lifecycleMu sync.Mutex
	closing     bool

	// statePersistMu and notesPersistMu serialise whole save operations,
	// snapshot included. Locking only the file write is not enough: two savers
	// can snapshot in one order and rename in the other, leaving the older
	// state on disk. The lock has to span taking the snapshot and writing it.
	statePersistMu sync.Mutex
	notesPersistMu sync.Mutex

	// configMu serialises read-modify-write cycles on config.json. Two
	// concurrent mutations each load, edit and save the whole file, so without
	// it the second save silently drops the first one's edit.
	configMu sync.Mutex

	// saveErr records the most recent persistence failure so the UI can say
	// that state is not reaching disk. A stateful session manager that fails
	// to persist in silence is telling the user a comfortable lie.
	saveErr error
}

func New(projects []*Project, drivers map[string]driver.Driver, statePath string) *Inbox {
	ctx, cancel := context.WithCancel(context.Background())
	return &Inbox{
		projects:    projects,
		drivers:     drivers,
		statePath:   statePath,
		claims:      claimsOf(statePath),
		cancels:     make(map[string]context.CancelFunc),
		active:      make(map[string]*activeTurn),
		done:        make(chan struct{}),
		bgCtx:       ctx,
		bgCancel:    cancel,
		gitNudge:    make(chan struct{}, 1),
		turnTimeout: config.DefaultTurnTimeout,
	}
}

// WithTurnTimeout bounds a single agent turn. Zero means no deadline.
func (in *Inbox) WithTurnTimeout(d time.Duration) *Inbox {
	in.mu.Lock()
	in.turnTimeout = d
	in.mu.Unlock()
	return in
}

// SaveErr reports the most recent state-persistence failure, or nil.
func (in *Inbox) SaveErr() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.saveErr
}

// NoteAttachReturned records that the user has been in this project's session
// interactively. Those turns went straight to the underlying CLI, so the
// history here is no longer the whole conversation — and a dashboard that
// silently implies it is will be trusted when it should not be.
func (in *Inbox) NoteAttachReturned(name string) {
	if name == "" {
		return
	}
	in.mu.Lock()
	p, err := in.projectByName(name)
	if err != nil {
		in.mu.Unlock()
		return
	}
	p.appendHistory(Message{
		Role:      "system",
		Content:   "interactive attach ended — the session may contain turns that are not shown here",
		Timestamp: time.Now(),
	})
	p.UpdatedAt = time.Now()
	in.mu.Unlock()
	in.save()
}

// Close stops the background watchers and waits for them, so nothing writes
// state after it returns. In-flight sends are cancelled rather than waited
// out — a turn can run for half an hour, and quitting should not.
//
// Safe to call more than once, and safe to call during a round.
func (in *Inbox) Close() {
	in.closeOnce.Do(func() {
		// Refuse new work before signalling, so that a watcher deciding to
		// schedule its next round loses the race rather than winning it.
		in.lifecycleMu.Lock()
		in.closing = true
		close(in.done)
		in.lifecycleMu.Unlock()
		// Background work is cancelled, not waited out. A git call has its own
		// five-second deadline, and quitting should not sit through it.
		in.bgCancel()
		// A scheduled wake must not fire into an inbox that is shutting down.
		in.oversight().Stop()

		in.mu.Lock()
		for name, cancel := range in.cancels {
			delete(in.cancels, name)
			cancel()
		}
		// Release anything waiting on a turn handle. A waiter selecting only
		// on Done would otherwise block until its own deadline.
		for name, t := range in.active {
			in.resolveTurn(name, t, TurnOutcome{Cancelled: true, Status: driver.StatusIdle})
		}
		in.mu.Unlock()
	})
	in.wg.Wait()
}

// track runs fn in a goroutine Close will wait for, reporting false when the
// inbox is shutting down and the work was refused. Callers scheduling
// follow-on work must treat a refusal as ordinary shutdown.
func (in *Inbox) track(fn func()) bool {
	in.lifecycleMu.Lock()
	if in.closing {
		in.lifecycleMu.Unlock()
		return false
	}
	// Add must happen under the same lock that Close checks, or Wait can
	// observe zero between the check and the Add.
	in.wg.Add(1)
	in.lifecycleMu.Unlock()

	go func() {
		defer in.wg.Done()
		fn()
	}()
	return true
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

// Group is one supervisor and the projects it oversees.
//
// Splitting a fleet is not organisation for its own sake. A supervisor's whole
// context is rebuilt each turn from its fleet's status lines and the notes that
// mention them, so a fleet of seven spends every turn describing four projects
// the question was not about. Two supervisors over three projects each are both
// cheaper and more accurate than one over six.
type Group struct {
	// Name labels the tab. Empty for the implicit single group, which has no
	// tab to label.
	Name string
	// King is the supervisor's project name.
	King string
	// Projects are the members, by name. Empty means this group takes whatever
	// is left over, which is how the single-supervisor case is expressed.
	Projects []string
}

// WithGroups sets the fleet's partition. Set once at startup.
//
// An empty partition is normalised to one anonymous group with no explicit
// members, so every reader downstream sees the same shape whether or not the
// user configured groups.
func (in *Inbox) WithGroups(groups []Group) *Inbox {
	in.mu.Lock()
	if len(groups) == 0 {
		groups = []Group{{}}
	}
	in.groups = append([]Group(nil), groups...)
	in.mu.Unlock()
	return in
}

// WithKing is the single-supervisor shorthand: one group, that king, and every
// other project as its fleet. Most installs are this, and they should not have
// to describe a partition in order to say so.
func (in *Inbox) WithKing(name string) *Inbox {
	return in.WithGroups([]Group{{King: name}})
}

// Groups returns a copy of the partition. Always at least one entry.
func (in *Inbox) Groups() []Group {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.groupsLocked()
}

func (in *Inbox) groupsLocked() []Group {
	if len(in.groups) == 0 {
		return []Group{{}}
	}
	out := make([]Group, len(in.groups))
	copy(out, in.groups)
	return out
}

// GroupCount is how many supervisors the fleet is split between.
func (in *Inbox) GroupCount() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.groupsLocked())
}

// KingNameOf is group g's supervisor, or "" when g is out of range or the
// group has no supervisor.
func (in *Inbox) KingNameOf(g int) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	gs := in.groupsLocked()
	if g < 0 || g >= len(gs) {
		return ""
	}
	return gs[g].King
}

// KingIndexOf resolves group g's supervisor to a 1-based project index, or 0
// when it is not in the list. Callers that hold an index across time must
// re-resolve: a removal shifts everything after it.
func (in *Inbox) KingIndexOf(g int) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	gs := in.groupsLocked()
	if g < 0 || g >= len(gs) || gs[g].King == "" {
		return 0
	}
	for i, p := range in.projects {
		if ident.SameName(p.Name, gs[g].King) {
			return i + 1
		}
	}
	return 0
}

// IsKing reports whether a project name belongs to any group's supervisor.
// Rendering asks this — "is this row a supervisor" — and the answer must not
// depend on which tab happens to be open.
func (in *Inbox) IsKing(name string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.isKingLocked(name)
}

func (in *Inbox) isKingLocked(name string) bool {
	for _, g := range in.groupsLocked() {
		if g.King != "" && ident.SameName(name, g.King) {
			return true
		}
	}
	return false
}

// FleetNamesOf is every project group g's supervisor may dispatch to.
//
// No supervisor is ever in its own fleet: a king that could send to itself
// would wait for a reply from the session that is waiting to send it. Neither
// is any other group's supervisor — the partition is the point.
//
// The first group also absorbs every project no group claimed. A project can
// be added at runtime, from the dashboard or by editing config, and a project
// that belonged to no supervisor would sit in the fleet unreachable and
// unasked. Landing it somewhere beats losing it.
func (in *Inbox) FleetNamesOf(g int) []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	gs := in.groupsLocked()
	if g < 0 || g >= len(gs) {
		return nil
	}

	claimed := make(map[string]bool)
	for _, grp := range gs {
		for _, n := range grp.Projects {
			claimed[ident.Name(n)] = true
		}
	}
	want := make(map[string]bool, len(gs[g].Projects))
	for _, n := range gs[g].Projects {
		want[ident.Name(n)] = true
	}

	out := make([]string, 0, len(in.projects))
	for _, p := range in.projects {
		if in.isKingLocked(p.Name) {
			continue
		}
		key := ident.Name(p.Name)
		if want[key] || (g == 0 && !claimed[key]) {
			out = append(out, p.Name)
		}
	}
	return out
}

// GroupOfProject is the group a project belongs to, or 0 for anything
// unclaimed — which is where FleetNamesOf puts it too. A supervisor reports
// the group it leads.
func (in *Inbox) GroupOfProject(name string) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	gs := in.groupsLocked()
	for i, g := range gs {
		if g.King != "" && ident.SameName(name, g.King) {
			return i
		}
		for _, n := range g.Projects {
			if ident.SameName(n, name) {
				return i
			}
		}
	}
	return 0
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

// addProject validates, persists, and only then mutates memory.
//
// The old order was the other way round: the project appeared in the UI and
// the config write was best-effort, so a failed write produced a project that
// worked until restart and then vanished. Config is the durable record of
// which projects exist, so a project that cannot be written there has not
// really been added.
func (in *Inbox) addProject(name, tool, dir, sessionID, forkFrom string) error {
	if err := ident.ValidateName(name); err != nil {
		return err
	}
	// The stored dir is the project's identity, so it is resolved to an
	// absolute path at creation. Persisting the string as typed meant a
	// relative dir like "." changed meaning with the caller's working
	// directory: a later one-shot command run from elsewhere could drive a
	// different repository while retaining the original project's session
	// identity (F15).
	resolved := ident.Dir(dir)
	if resolved == "" {
		return fmt.Errorf("project directory %q could not be resolved", dir)
	}
	dir = resolved
	in.mu.Lock()
	for _, p := range in.projects {
		if ident.SameName(p.Name, name) {
			in.mu.Unlock()
			return fmt.Errorf("a project named %q already exists (dir: %s)", name, p.Dir)
		}
		if ident.SameDir(p.Dir, dir) {
			in.mu.Unlock()
			return fmt.Errorf("directory %q is already used by project %q", dir, p.Name)
		}
	}
	if in.isKingLocked(name) {
		in.mu.Unlock()
		return fmt.Errorf("%q is reserved for a supervisor", name)
	}
	if _, ok := in.drivers[tool]; !ok {
		in.mu.Unlock()
		return fmt.Errorf("unknown tool %q (no driver registered)", tool)
	}
	in.mu.Unlock()

	if in.configPath != "" {
		if err := in.updateConfig(func(s *config.Settings) error {
			if !s.AddProject(config.Project{Name: name, Tool: tool, Dir: dir}) {
				return fmt.Errorf("config already lists a project with that name or directory")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("cannot add %s: %w", name, err)
		}
	}

	in.mu.Lock()
	in.projects = append(in.projects, &Project{
		Name:      name,
		Tool:      tool,
		Dir:       dir,
		SessionID: sessionID,
		ForkFrom:  forkFrom,
		Status:    driver.StatusIdle,
	})
	in.mu.Unlock()
	// An explicit add outranks an old deletion: without this, re-adding a
	// removed project would leave its tombstone in place and every
	// subsequent merge would drop its state again.
	untombstoneProject(in.statePath, name)
	in.save()
	return nil
}

// updateConfig applies fn to the on-disk config and saves it, serialised
// against other config mutations. Returns the error rather than logging it:
// the caller has in-memory state to keep consistent with the result.
//
// The serialisation is cross-process as well as in-process: a harness adding
// a project and the dashboard doing anything config-touching are two programs
// doing load-edit-save on one file, and the second save would silently drop
// the first one's edit.
func (in *Inbox) updateConfig(fn func(*config.Settings) error) error {
	in.configMu.Lock()
	defer in.configMu.Unlock()
	if in.configPath == "" {
		return fmt.Errorf("no config path")
	}
	return fsutil.WithFileLock(in.configPath+".lock", lockWait, func() error {
		settings, err := config.Load(in.configPath)
		if err != nil {
			return fmt.Errorf("cannot safely write config: load failed: %w", err)
		}
		if err := fn(settings); err != nil {
			return err
		}
		// Validate before saving: a mutation that leaves an inconsistent
		// structure (a group naming a project that no longer exists, for
		// instance) must not reach disk, because the next startup validates
		// and refuses to run — a UI action would have bricked the config
		// (F16). Failing here keeps the original file intact.
		if err := config.Validate(settings); err != nil {
			return fmt.Errorf("config change rejected (original kept): %w", err)
		}
		return config.Save(in.configPath, settings)
	})
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
	// The canonical identity predicate, not strings.EqualFold: the two
	// disagree for Unicode fold pairs (s and ſ), which let a name that
	// uniqueness-validated as distinct resolve to the wrong project.
	for _, p := range in.projects {
		if ident.SameName(p.Name, name) {
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
	_, err := in.startSend(resolve, displayText, driverText, record)
	return err
}

// startSend begins a turn and returns a handle that resolves when that exact
// turn finishes. Callers who only want the side effect discard the handle; the
// supervisor keeps it, because "the turn I started" and "the next time this
// project is idle" are not the same event.
func (in *Inbox) startSend(resolve func() (*Project, error), displayText, driverText string, record bool) (TurnHandle, error) {
	return in.startSendTimed(resolve, displayText, driverText, record, 0)
}

// startSendTimed is startSend with a per-turn timeout override. Zero keeps the
// inbox's configured bound; the headless front-end passes its own so a
// caller that stops waiting also stops paying.
func (in *Inbox) startSendTimed(resolve func() (*Project, error), displayText, driverText string, record bool, timeout time.Duration) (TurnHandle, error) {
	return in.startSendTimedCtx(context.Background(), resolve, displayText, driverText, record, timeout)
}

// startSendTimedCtx is startSendTimed with the turn's context derived from a
// caller's: cancelling the request cancels the subprocess, so a disconnected
// MCP client or an abandoned CLI pipe does not leave the agent spending in
// the background (F11).
func (in *Inbox) startSendTimedCtx(parent context.Context, resolve func() (*Project, error), displayText, driverText string, record bool, timeout time.Duration) (TurnHandle, error) {
	// First pass under the lock: validate the target and capture what the
	// claim needs. The claim itself is taken with no lock held, because a
	// claim held by this same process is released by a goroutine that may be
	// milliseconds away, and waiting for it under the inbox mutex would stall
	// every snapshot and render in the meantime.
	in.mu.Lock()
	p, err := resolve()
	if err != nil {
		in.mu.Unlock()
		return TurnHandle{}, err
	}
	if p.Status == driver.StatusWorking {
		in.mu.Unlock()
		return TurnHandle{}, fmt.Errorf("%s is already working", p.Name)
	}
	if _, ok := in.drivers[p.Tool]; !ok {
		in.mu.Unlock()
		return TurnHandle{}, fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	claimName, claimTool := ident.Name(p.Name), p.Tool
	// Pin the target's identity, not its slot: the second pass re-resolves
	// by name and verifies it is still the same project object with the same
	// tool and directory. The numeric resolver used to be called twice, so a
	// removal between the passes shifted the index and delivered the prompt
	// to a different project while holding the original's claim.
	target, targetName, targetDir := p, p.Name, p.Dir
	targetDefinition := *p
	in.mu.Unlock()

	turnKey := newTurnKey()
	if err := in.acquireClaim(claimName, claimTool, turnKey); err != nil {
		return TurnHandle{}, err
	}
	// We own the project now, so the session to resume is whatever the last
	// turn left on disk — not the snapshot this process loaded. A long-lived
	// inbox that acquires a project another process just finished would
	// otherwise send with a stale session id, fork source and history, and
	// its local turn would make the merge prefer that stale snapshot.
	in.refreshOwnedProject(claimName)
	// Ownership serializes sends; it does not make this process's cached
	// configuration correct. A frontend that loaded the project before
	// another changed its tool or directory must refuse rather than drive
	// the old identity against the user's current config.
	if err := in.requireCurrentDefinitions([]Project{targetDefinition}); err != nil {
		return TurnHandle{}, errors.Join(err, in.claims.Release(claimName, turnKey))
	}

	// Second pass: everything may have moved while the lock was down — the
	// project removed, its tool changed, another send started. Re-resolve by
	// the pinned name and verify identity; the claim is already ours, so a
	// competing send lost at the filesystem instead of here.
	in.mu.Lock()
	p, err = in.projectByName(targetName)
	if err == nil && (p != target || p.Tool != claimTool || !ident.SameDir(p.Dir, targetDir)) {
		err = fmt.Errorf("project %q changed while acquiring ownership; retry", targetName)
	}
	if err != nil {
		in.mu.Unlock()
		return TurnHandle{}, errors.Join(err, in.claims.Release(claimName, turnKey))
	}
	d, ok := in.drivers[p.Tool]
	if !ok {
		in.mu.Unlock()
		in.claims.Release(claimName, turnKey)
		return TurnHandle{}, fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	if p.Status == driver.StatusWorking {
		// Superseded between the passes by a send that did win the claim.
		in.mu.Unlock()
		in.claims.Release(claimName, turnKey)
		return TurnHandle{}, fmt.Errorf("%s is already working", p.Name)
	}
	p.Status = driver.StatusWorking
	p.LastErr = ""
	// Sending is the answer to whatever it was blocked on, or at least
	// supersedes it. A reason left behind would keep the badge up through the
	// next turn and the one after.
	p.WaitReason, p.WaitDetail = "", ""
	// The new turn's trace starts clean; the previous one's is only worth
	// keeping until something newer begins.
	p.Trace = nil
	p.UpdatedAt = time.Now()
	// Append the DISPLAY text (user's original message) to history —
	// NOT the driverText which may include injected state context.
	if record {
		p.appendHistory(Message{Role: "user", Content: displayText, Timestamp: time.Now()})
	}
	dir, sid, forkFrom, name := p.Dir, p.SessionID, p.ForkFrom, p.Name

	// A deadline bounds a CLI that has stopped making progress. It is
	// configurable because a coding agent running a build legitimately takes
	// longer than any fixed guess, and the old five minutes killed real work
	// and reported it as failure. An explicit timeout (the headless
	// front-end's) overrides the configured one. The context derives from
	// the caller's, so cancelling the request cancels the work underneath.
	ctx, cancel := context.WithCancel(parent)
	bound := timeout
	if bound <= 0 {
		bound = in.turnTimeout
	}
	if bound > 0 {
		ctx, cancel = context.WithTimeout(ctx, bound)
	}
	in.cancels[name] = cancel
	turn, handle := in.beginTurn(name, cancel)
	in.mu.Unlock()
	in.save()

	started := in.track(func() {
		defer cancel()
		// Send the DRIVER text (may include injected state) to the CLI.
		fd, canFork := d.(driver.ForkingDriver)
		sd, canStream := d.(driver.StreamingDriver)
		switch {
		case forkFrom != "" && canFork:
			// The fork turn does not stream. It is one turn, it happens once
			// per adoption, and giving it a second code path through the
			// streaming interface would double the surface for a spinner.
			in.blockingSend(p, turn, func() driver.Result {
				return fd.SendForked(ctx, dir, forkFrom, driverText)
			})
		case forkFrom != "":
			// Nothing to fork with: start clean rather than resuming an id
			// this driver has no claim to.
			in.blockingSend(p, turn, func() driver.Result { return d.Send(ctx, dir, "", driverText) })
		case canStream:
			in.streamSend(ctx, sd, p, turn, dir, sid, driverText)
		default:
			in.blockingSend(p, turn, func() driver.Result { return d.Send(ctx, dir, sid, driverText) })
		}
		in.mu.Lock()
		// Only clear the cancel func if this turn is still the current one. A
		// superseded turn finishing late would otherwise delete the entry
		// belonging to the turn that replaced it, making the new turn
		// uncancellable.
		if in.isCurrentTurn(name, turn) {
			delete(in.cancels, name)
		}
		in.mu.Unlock()
		// Persist the finished turn. streamSend saves as events arrive, but
		// blockingSend — every non-streaming driver, which is opencode, codex
		// and mock — only mutated memory. Its reply reached the screen and
		// then vanished on the next restart.
		//
		// A successful turn whose result could not be committed is not a
		// success the caller may treat as durable: the handle resolving must
		// say so (F19). The result stays in memory and the next successful
		// save commits it, but a headless caller told "done" would exit and
		// drop it on the floor.
		if err := in.save(); err != nil {
			in.mu.Lock()
			if in.isCurrentTurn(name, turn) && turn.outcome != nil && turn.outcome.Err == nil {
				o := *turn.outcome
				o.Err = fmt.Errorf("reply not persisted: %w", err)
				turn.outcome = &o
				if p, perr := in.projectByName(name); perr == nil {
					p.LastErr = o.Err.Error()
				}
			}
			in.mu.Unlock()
		}
		// An agent turn is the one thing this program does that moves a
		// project's tree, so it is the moment the branch and dirty state on
		// screen are most likely to be stale.
		in.nudgeGit()
		// The claim is released only now, with the turn fully filed: the
		// filesystem guard must not come off while our own write is still in
		// flight. A superseding turn holds a different key and is unaffected.
		// Release exactly the resource acquired — the captured claim name,
		// not a name re-resolved after the fact — and carry the failure into
		// the outcome: a turn whose ownership may still be held is not a
		// clean completion, and later sends would stay blocked with nobody
		// told why.
		releaseErr := in.claims.Release(claimName, turnKey)
		if releaseErr != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: claim release/cleanup failed: %v\n", releaseErr)
		}
		// Resolve last, after the save and the release: a caller woken by the
		// handle can treat the turn as durable — on disk, claim off — rather
		// than racing this goroutine's bookkeeping. A turn that was
		// superseded was resolved by whatever superseded it, and a turn with
		// no outcome (the subprocess never returned one) resolves as an
		// error so no waiter is left holding a handle that never fires.
		in.mu.Lock()
		if in.isCurrentTurn(name, turn) {
			outcome := TurnOutcome{
				Status: driver.StatusError,
				Err:    fmt.Errorf("turn ended without filing an outcome"),
			}
			if turn.outcome != nil {
				outcome = *turn.outcome
			}
			if releaseErr != nil {
				cleanupErr := fmt.Errorf("claim release/cleanup failed: %w", releaseErr)
				outcome.Err = errors.Join(outcome.Err, cleanupErr)
				in.saveErr = errors.Join(in.saveErr, cleanupErr)
				if current, cerr := in.projectByName(name); cerr == nil {
					current.LastErr = outcome.Err.Error()
				}
			}
			in.resolveTurn(name, turn, outcome)
		}
		in.mu.Unlock()
	})
	if !started {
		// Shutting down. Resolve the handle so nobody waits on a turn that
		// will never run.
		in.mu.Lock()
		in.resolveTurn(name, turn, TurnOutcome{Cancelled: true, Status: driver.StatusIdle})
		delete(in.cancels, name)
		p.Status = driver.StatusIdle
		in.mu.Unlock()
		cancel()
		if rerr := in.claims.Release(claimName, turnKey); rerr != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: claim release/cleanup failed: %v\n", rerr)
		}
		return TurnHandle{}, fmt.Errorf("shutting down")
	}
	return handle, nil
}

// blockingSend runs a non-streaming turn and files its outcome. run is what
// actually talks to the CLI — an ordinary send, or a fork of somebody else's
// session — so the three ways to start a turn share one way to end it.
//
// It files into memory and records the outcome; it does not resolve the
// handle. Resolution belongs to the turn goroutine, after the save, so that
// anybody woken by the handle can read the result from disk.
func (in *Inbox) blockingSend(p *Project, turn *activeTurn, run func() driver.Result) {
	res := run()
	in.mu.Lock()
	defer in.mu.Unlock()
	// A turn that is no longer the current one has been cancelled or
	// superseded. Its subprocess was killed and its result describes a turn
	// nobody is waiting for; applying it would overwrite the turn that
	// replaced it.
	if !in.isCurrentTurn(p.Name, turn) {
		return
	}
	// A session id is only adopted when the turn succeeded. On the fork path
	// the driver's fallback id is the *source* session — somebody else's live
	// agent — so taking it from a failed turn left the project holding a
	// session it does not own and must not resume.
	if res.Err == nil && res.SessionID != "" {
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
	out := TurnOutcome{SessionID: p.SessionID, Status: res.Status, Err: res.Err}
	if res.Err != nil {
		p.LastErr = res.Err.Error()
		p.appendHistory(Message{Role: "error", Content: res.Err.Error(), Timestamp: time.Now()})
	} else {
		p.LastErr = ""
		p.LastMessage = res.Final
		p.appendHistory(Message{Role: "assistant", Content: res.Final, Timestamp: time.Now()})
		out.Final = res.Final
	}
	p.UpdatedAt = time.Now()
	p.bumpRevision()
	turn.outcome = &out
}

// streamSend consumes a StreamingDriver's event channel and updates the
// project's state live. Emits one final assistant (or error) history entry
// when the turn completes, identical to the blocking path's behavior.
//
// Must be called from a background goroutine (it is — by Send's caller).
// Holds the inbox mutex briefly per event to mutate Project state.
func (in *Inbox) streamSend(ctx context.Context, sd driver.StreamingDriver, p *Project, turn *activeTurn, dir, sid, prompt string) {
	ch := sd.StreamSend(ctx, dir, sid, prompt)

	for ev := range ch {
		in.mu.Lock()
		// Cancelled or superseded: stop processing events for a turn nobody
		// is waiting on.
		if !in.isCurrentTurn(p.Name, turn) {
			in.mu.Unlock()
			// Drain rather than return, so the driver's sender is not left
			// blocked writing to a channel with no reader.
			for range ch {
			}
			return
		}
		// A terminal event has already been filed for this turn: a late
		// progress event must not flip the project back to working and
		// block the next send. Keep draining, but freeze the outcome.
		if turn.outcome != nil {
			in.mu.Unlock()
			continue
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
			// Saved, unlike a plain Activity update: the trace is persisted
			// state, and saving per tool call is what makes a mid-turn
			// `status` show progress — the difference between "working" and
			// "working, twelve tools in, last one Edit". Tool calls are
			// seconds apart, not a stream of their own.
			p.appendTrace(ev.Activity)
			p.UpdatedAt = time.Now()
			in.mu.Unlock()
			in.save()
			continue
		case driver.StreamDone:
			p.Status = driver.StatusWaiting
			p.Activity = ""
			p.StreamingText = ""
			p.LastErr = ""
			p.LastMessage = ev.Content
			p.appendHistory(Message{Role: "assistant", Content: ev.Content, Timestamp: time.Now()})
			p.UpdatedAt = time.Now()
			p.bumpRevision()
			turn.outcome = &TurnOutcome{
				SessionID: p.SessionID,
				Final:     ev.Content,
				Status:    driver.StatusWaiting,
			}
			in.mu.Unlock()
			in.save()
			continue
		case driver.StreamError:
			msg := "turn failed"
			if ev.Err != nil {
				msg = ev.Err.Error()
			}
			in.finishStreamErrorLocked(p, turn, msg, ev.Content)
			in.mu.Unlock()
			in.save()
			continue
		}
		p.UpdatedAt = time.Now()
		in.mu.Unlock()
		in.save()
	}

	// The channel closed without a terminal event. That is a failure, and it
	// has to be filed the same way an explicit StreamError is: the old code
	// set the status and stopped, leaving StreamingText populated but no
	// longer rendered, LastMessage still showing the *previous* turn, and
	// nothing at all in history to say what happened.
	//
	// Resolution is the turn goroutine's now, so "the handle has not
	// resolved" is no longer evidence of a missing terminal event — the
	// outcome being nil is.
	in.mu.Lock()
	if in.isCurrentTurn(p.Name, turn) && turn.outcome == nil {
		in.finishStreamErrorLocked(p, turn, "stream ended without completion event", "")
		in.mu.Unlock()
		in.save()
		return
	}
	in.mu.Unlock()
}

// finishStreamErrorLocked files a failed streaming turn. Caller must hold mu.
//
// One routine for every way a stream can fail, because the ways it can fail
// kept acquiring slightly different behaviour: partial text preserved in one
// path and dropped in another, history appended here and not there.
func (in *Inbox) finishStreamErrorLocked(p *Project, turn *activeTurn, msg, evContent string) {
	partial := strings.TrimSpace(p.StreamingText)
	p.Status = driver.StatusError
	p.Activity = ""
	p.LastErr = msg
	switch {
	case partial != "":
		p.LastMessage = partial
		p.appendHistory(Message{Role: "assistant", Content: partial + "\n\n(error: " + msg + ")", Timestamp: time.Now()})
	case evContent != "":
		p.LastMessage = evContent
		p.appendHistory(Message{Role: "error", Content: evContent + "\n\n(error: " + msg + ")", Timestamp: time.Now()})
	default:
		p.appendHistory(Message{Role: "error", Content: msg, Timestamp: time.Now()})
	}
	p.StreamingText = ""
	p.UpdatedAt = time.Now()
	p.bumpRevision()
	turn.outcome = &TurnOutcome{
		SessionID: p.SessionID,
		Partial:   partial,
		Status:    driver.StatusError,
		Err:       errors.New(msg),
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
			// No local turn: if a live claim exists, another frontend owns
			// this work and reporting success here would falsely dismiss
			// it while it keeps running.
			if info, held := in.claims.Peek(ident.Name(p.Name)); held {
				name := p.Name
				in.mu.Unlock()
				return fmt.Errorf(
					"%s is being driven by agent-inbox pid %d; cancel it from its owning frontend",
					name, info.Pid)
			}
			// Defensive: status says working but no cancel func and no
			// owner. Treat as a stuck state and reset to idle.
			p.Status = driver.StatusIdle
			p.Activity = ""
			p.LastErr = "stuck (no cancel func); reset to idle"
			if t, live := in.active[p.Name]; live {
				in.resolveTurn(p.Name, t, TurnOutcome{Cancelled: true, Status: driver.StatusIdle})
			}
			in.mu.Unlock()
			in.save()
			return nil
		}
		delete(in.cancels, p.Name)
		p.Status = driver.StatusIdle
		p.Activity = ""
		p.StreamingText = ""
		p.LastErr = "cancelled by user"
		p.appendHistory(Message{Role: "system", Content: "cancelled by user", Timestamp: time.Now()})
		p.UpdatedAt = time.Now()
		// Resolve the handle explicitly. A waiter left polling an idle project
		// learns nothing from the status change and waits out its whole
		// deadline before reporting a timeout that already happened.
		if t, live := in.active[p.Name]; live {
			in.resolveTurn(p.Name, t, TurnOutcome{Cancelled: true, Status: driver.StatusIdle})
		}
		in.mu.Unlock()
		in.save()

		cancel() // signal the subprocess to die; goroutine will no-op on return
		return nil

	case driver.StatusWaiting, driver.StatusError:
		// Dismiss the notification — agent already finished, just reset status.
		previous := p.Status
		p.Status = driver.StatusIdle
		p.Activity = ""
		p.WaitReason, p.WaitDetail = "", ""
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
	// A supervisor is not one of the projects you manage — it is the thing
	// managing them. Removing it would leave a tab whose main view has no
	// conversation to show and no way to get one back.
	if in.isKingLocked(name) {
		in.mu.Unlock()
		return fmt.Errorf("%s is a supervisor — it cannot be removed", name)
	}
	in.mu.Unlock()

	// Durable removal first. The old order deleted the project and its notes
	// and then tried to write config; a failed write brought the project back
	// on restart with its notes already gone.
	if in.configPath != "" {
		if err := in.updateConfig(func(s *config.Settings) error {
			s.RemoveProject(name)
			return nil
		}); err != nil {
			return fmt.Errorf("cannot remove %s: %w", name, err)
		}
	}

	in.mu.Lock()
	// Re-resolve: the index may have shifted while the config was being
	// written.
	p, err = in.projectByName(name)
	if err != nil {
		in.mu.Unlock()
		return nil // already gone
	}
	// Cancel any in-flight send before removing.
	if cancel, ok := in.cancels[name]; ok {
		delete(in.cancels, name)
		cancel()
	}
	if t, live := in.active[name]; live {
		in.resolveTurn(name, t, TurnOutcome{Cancelled: true, Status: driver.StatusIdle})
	}
	for i, q := range in.projects {
		if q == p {
			in.projects = append(in.projects[:i], in.projects[i+1:]...)
			break
		}
	}
	in.mu.Unlock()
	in.save()
	// Durable, explicit removal: the state entry goes first, then the
	// tombstone that keeps a stale writer's next merge from resurrecting it.
	in.deleteStateEntry(name)
	tombstoneProject(in.statePath, name)
	in.forgetProject(name)
	return nil
}

// SetProjectTool changes the driver for project idx (1-based). Clears the
// session id (a Claude session can't be resumed by OpenCode, etc.).
//
// A tool change is a project mutation like a send and is serialised against
// sends the same way: through the filesystem claim. The old in-process check
// released the inbox lock while rewriting config, so a king-dispatched send
// could start in that gap, run on the old driver, and file an old-tool
// session id into the project after the switch cleared it.
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
	// Selecting the tool the project already uses is not a change, and the
	// unconditional reset below would silently destroy the session it is
	// mid-conversation with. Nothing to persist, nothing to clear.
	if p.Tool == tool {
		in.mu.Unlock()
		return nil
	}
	name, from := p.Name, p.Tool
	in.mu.Unlock()

	claimName := ident.Name(name)
	turnKey := newTurnKey()
	if err := in.claims.Acquire(claimName, turnKey, tool); err != nil {
		var held *claim.HeldError
		if errors.As(err, &held) {
			return fmt.Errorf("%s is currently working — cancel before changing tool", name)
		}
		return err
	}
	defer in.claims.Release(claimName, turnKey)

	// Validate the in-memory state BEFORE any durable mutation, and hold the
	// mutex across both the check and the config write: writing config first
	// and refusing afterwards left disk and memory inconsistent, with later
	// sends blocked by the definition guard over a change the caller was
	// told had failed.
	in.mu.Lock()
	p, err = in.projectByName(name)
	if err != nil {
		in.mu.Unlock()
		return err
	}
	if p.Status == driver.StatusWorking {
		in.mu.Unlock()
		return fmt.Errorf("%s is currently working — cancel before changing tool", name)
	}
	if in.configPath != "" {
		if err := in.updateConfig(func(s *config.Settings) error {
			s.SetProjectTool(name, tool)
			return nil
		}); err != nil {
			in.mu.Unlock()
			return fmt.Errorf("cannot change %s to %s: %w", name, tool, err)
		}
	}
	p.Tool = tool
	p.SessionID = "" // previous session is meaningless to the new tool
	p.ForkFrom = ""  // and so is a fork source belonging to the old one
	p.Status = driver.StatusIdle
	p.Activity = ""
	// Retained history is about to look like one continuous conversation with
	// a tool that has never seen any of it. Mark the seam.
	p.appendHistory(Message{
		Role:      "system",
		Content:   fmt.Sprintf("tool changed from %s to %s; a new session will start", from, tool),
		Timestamp: time.Now(),
	})
	p.UpdatedAt = time.Now()
	in.mu.Unlock()
	in.save()
	return nil
}

const (
	// maxHistoryMessages bounds how many turns are kept per project.
	maxHistoryMessages = 100
	// maxMessageBytes bounds a single stored message. A count-only limit let
	// one project's state grow without bound: a hundred messages is small
	// until one of them is a megabyte of build output, and every save and
	// every startup then pays for it.
	maxMessageBytes = 256 * 1024
	// maxHistoryBytes bounds a project's whole retained history.
	maxHistoryBytes = 4 * 1024 * 1024
)

// appendHistory stores a message, bounding both the number of messages and
// the bytes they occupy.
func (p *Project) appendHistory(m Message) {
	m.Content = truncateBytes(m.Content, maxMessageBytes)
	p.History = append(p.History, m)
	if len(p.History) > maxHistoryMessages {
		p.History = p.History[len(p.History)-maxHistoryMessages:]
	}
	total := 0
	for _, h := range p.History {
		total += len(h.Content)
	}
	// Drop oldest until the budget is met, but always keep the newest message
	// — a history of nothing is less useful than a history of one.
	for total > maxHistoryBytes && len(p.History) > 1 {
		total -= len(p.History[0].Content)
		p.History = p.History[1:]
	}
}

// truncateBytes caps s at n bytes without splitting a rune, marking that it
// did so. Silent truncation of an agent's reply reads as the agent stopping
// early.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "\n\n[truncated]"
	cut := n - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
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

// AttachLease is a project's ownership held for one interactive attach. The
// claim refuses managed sends from every other process while the user is in
// the session — without it, a managed send could start against the same
// session mid-attach and interleave a subprocess's transcript with the user's
// own typing.
type AttachLease struct {
	set  *claim.Set
	name string
	key  string
}

// Release drops the attach claim, reporting failure. Call it when the
// interactive child exits (and on any failure to start it): a lease left
// held blocks managed sends until the holder's process dies, so a release
// that failed must be said rather than swallowed. Safe on a nil lease.
func (l *AttachLease) Release() error {
	if l == nil {
		return nil
	}
	return l.set.Release(l.name, l.key)
}

// BeginAttach validates attachment and takes the project claim for the
// interactive run's lifetime, returning the argv, the working dir, and the
// lease. The caller must hold the lease until the foreground child is reaped
// and Release it then (and on any failure to start). The claim also refreshes
// the session under ownership, like a send does — the attach runs against
// the session on disk, not a stale snapshot.
func (in *Inbox) BeginAttach(idx int) ([]string, string, *AttachLease, error) {
	in.mu.Lock()
	p, err := in.project(idx)
	if err != nil {
		in.mu.Unlock()
		return nil, "", nil, err
	}
	if _, ok := in.drivers[p.Tool]; !ok {
		in.mu.Unlock()
		return nil, "", nil, fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	name := p.Name
	targetDefinition := *p
	in.mu.Unlock()

	claimName := ident.Name(name)
	turnKey := newTurnKey()
	if err := in.acquireClaim(claimName, "", turnKey); err != nil {
		var held *claim.HeldError
		if errors.As(err, &held) {
			return nil, "", nil, fmt.Errorf("%s is working — cancel or wait before attaching", name)
		}
		return nil, "", nil, err
	}
	lease := &AttachLease{set: in.claims, name: claimName, key: turnKey}
	release := func() {
		if err := lease.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "agent-inbox: attach claim cleanup failed: %v\n", err)
		}
	}
	if err := in.requireCurrentDefinitions([]Project{targetDefinition}); err != nil {
		release()
		return nil, "", nil, err
	}

	in.refreshOwnedProject(claimName)

	in.mu.Lock()
	defer in.mu.Unlock()
	p, err = in.projectByName(name)
	if err != nil {
		release()
		return nil, "", nil, err
	}
	// Attaching to a session the inbox is mid-turn on puts two writers on one
	// conversation. Delete and tool-change already refuse this; attach did
	// not, and it is the one that hands the user an interactive prompt into a
	// session a subprocess is still writing to.
	if p.Status == driver.StatusWorking {
		release()
		return nil, "", nil, fmt.Errorf("%s is working — cancel or wait before attaching", p.Name)
	}
	// A pending fork source belongs to somebody else's live agent. Attaching
	// to it would drop the user into that agent's session, not this project's.
	if p.ForkFrom != "" {
		release()
		return nil, "", nil, fmt.Errorf("%s has not forked its adopted session yet — send it a message first", p.Name)
	}
	if p.SessionID == "" {
		release()
		return nil, "", nil, fmt.Errorf("%s has no session yet — send it a message first", p.Name)
	}
	d, ok := in.drivers[p.Tool]
	if !ok {
		release()
		return nil, "", nil, fmt.Errorf("%s: no driver for tool %q", p.Name, p.Tool)
	}
	return d.AttachArgs(p.Dir, p.SessionID), p.Dir, lease, nil
}

// save writes state.json atomically, merging with whatever other processes
// have written rather than overwriting it — see multi.go for why a wholesale
// snapshot is no longer sound.
//
// The persist lock is taken before the snapshot, not just around the write.
// Atomic rename alone does not order two savers: one can snapshot old state,
// be descheduled while a second snapshots and writes newer state, then wake up
// and rename its stale copy over the top. The file stays valid JSON and the
// state goes backwards.
//
// It returns the persistence error rather than only recording it: a caller
// that is about to report a successful turn must be able to tell "committed"
// from "ran but not committed" (F19).
func (in *Inbox) save() error {
	in.statePersistMu.Lock()
	defer in.statePersistMu.Unlock()
	if in.closed() || in.statePath == "" {
		return nil
	}
	err := fsutil.WithFileLock(in.statePath+".lock", lockWait, in.mergeState)
	in.recordSaveErr(err)
	return err
}

func (in *Inbox) recordSaveErr(err error) {
	in.mu.Lock()
	in.saveErr = err
	in.mu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: state not saved: %v\n", err)
	}
}

// LoadState overlays persisted session ids and last messages onto the project
// set defined by config.
//
// A saved entry is only applied when its whole identity matches: name, tool
// and directory. Matching on the name alone meant that editing a project in
// config — pointing it at a different repository, or switching it to a
// different CLI — handed the new project the old one's session id and
// conversation. The session then resumed with another repository's context.
func LoadState(path string, projects []*Project) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var saved []Project
	if json.Unmarshal(b, &saved) != nil {
		// Damaged, not absent: the sessions on disk were not loaded, and
		// starting blank without a word would present that as normal.
		fmt.Fprintf(os.Stderr, "agent-inbox: state file %s is damaged; sessions were not restored\n", path)
		return
	}
	claims := claimsOf(path)
	byName := make(map[string]Project, len(saved))
	for _, s := range saved {
		byName[ident.Name(s.Name)] = s
	}
	for _, p := range projects {
		s, ok := byName[ident.Name(p.Name)]
		if !ok {
			continue
		}
		if !sameProject(s, *p) {
			continue // different or ambiguous identity — start fresh
		}
		p.SessionID = s.SessionID
		// Without this an adoption that was never sent to before a restart
		// comes back as a blank project, which is the one case the field was
		// persisted for.
		p.ForkFrom = s.ForkFrom
		p.Status = s.Status
		p.LastMessage = s.LastMessage
		p.LastErr = s.LastErr
		p.WaitReason = s.WaitReason
		p.WaitDetail = s.WaitDetail
		p.UpdatedAt = s.UpdatedAt
		p.History = s.History
		p.Trace = s.Trace
		// The change counter and the event-dedup markers are durable state:
		// dropping them at startup reset follow's baseline to zero (an
		// artificial wake from the first refresh), restarted the revision
		// sequence, and forgot which spool files were already applied.
		p.Revision = s.Revision
		p.SeenEvents = append([]string(nil), s.SeenEvents...)
		if p.Status == driver.StatusWorking {
			// A persisted Working status belongs to whichever process was
			// mid-turn when it was written. Loading it here is not
			// necessarily that process restarting — a one-shot status or
			// follow reader is usually an observer — so the claim decides:
			// held by a live pid, somebody is genuinely mid-turn and the
			// status stands; held by nobody, the owner died mid-turn and the
			// project is idle again, with a note saying the turn was
			// interrupted rather than cleanly finished (F20).
			if _, held := claims.Peek(ident.Name(p.Name)); !held {
				p.Status = driver.StatusIdle
				p.LastErr = "turn interrupted: the process driving it is gone"
				// The reason belonged to that turn, not to the idle project
				// left behind.
				p.WaitReason, p.WaitDetail = "", ""
			}
		}
	}
}
