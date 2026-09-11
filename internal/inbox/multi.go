package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/im-tyler/agent-inbox/internal/claim"
	"github.com/im-tyler/agent-inbox/internal/driver"
	"github.com/im-tyler/agent-inbox/internal/fsutil"
	"github.com/im-tyler/agent-inbox/internal/git"
	"github.com/im-tyler/agent-inbox/internal/ident"
)

// More than one process at a time.
//
// The Inbox was built as the single owner of state.json: everything it knew
// lived in its memory, and save() wrote that memory out wholesale. That was
// sound when the only writer was the dashboard. It is not now — a
// harness-driven `agent-inbox send` and an MCP tool call are whole processes
// of their own — and a full-snapshot save from a process holding stale memory
// silently reverts whatever the other one just did: a session id unwound, a
// reply filed under a turn that never happened.
//
// Three rules make coexistence sound, and all of them live here:
//
//   - Saves merge rather than overwrite. Under a cross-process lock, each
//     project entry is resolved independently: a locally active turn always
//     wins, and otherwise the newer UpdatedAt does. Two processes touching
//     different projects both survive; two touching the same one cannot,
//     because a send claims the project first.
//   - Sends claim. The in-process "already working" guard cannot see another
//     process, so the same guard is taken in the filesystem where both can
//     see it (internal/claim).
//   - Mutations of shared stores are read-modify-write under the lock, on
//     every side. Notes and config are re-read from disk inside the lock
//     before being changed, so the last operation wins rather than the last
//     process to have loaded a file.

// lockWait bounds contention for the state lock. Holders do millisecond-scale
// file work; the only way to wait this long is a holder that died without
// closing a descriptor, which the kernel handles, so a timeout here is
// reported loudly rather than waited out.
const lockWait = 10 * time.Second

// claimsOf returns the claim set for the state file's directory. An empty
// state path (session-only inboxes in tests) has nowhere to put a claim, and
// no other process to guard against, so it gets none.
func claimsOf(statePath string) *claim.Set {
	if statePath == "" {
		return nil
	}
	return claim.New(filepath.Join(filepath.Dir(statePath), "claims"))
}

// mergeState writes our state to disk, entry by entry, under the cross-process
// lock. See the rules at the top of this file.
//
// The persist lock still spans the snapshot — see Inbox.save — because two
// in-process savers remain as possible as they ever were.
func (in *Inbox) mergeState() error {
	// Snapshot ours under mu, along with which projects have a local turn
	// in flight. Those entries are ours to write regardless of timestamps:
	// nobody else can be mid-turn on them (a send claims first), and the
	// turn's own writes must not be second-guessed by a concurrently filed
	// external status.
	in.mu.Lock()
	ours := make([]Project, len(in.projects))
	for i, p := range in.projects {
		ours[i] = *p
	}
	localTurn := make(map[string]bool, len(in.active))
	for name := range in.active {
		localTurn[ident.Name(name)] = true
	}
	in.mu.Unlock()

	disk := readStateFile(in.statePath)
	diskByName := make(map[string]Project, len(disk))
	for _, d := range disk {
		diskByName[ident.Name(d.Name)] = d
	}

	out := make([]Project, 0, len(ours)+len(disk))
	written := make(map[string]bool, len(ours))
	for i := range ours {
		p := ours[i]
		key := ident.Name(p.Name)
		written[key] = true
		if d, ok := diskByName[key]; ok && !localTurn[key] && sameProject(d, p) && d.UpdatedAt.After(p.UpdatedAt) {
			adoptPersisted(&p, d)
		}
		out = append(out, p)
	}
	// Entries only the other process knows about are kept, not dropped: a
	// merge must never delete, because "absent from our memory" is also the
	// state of a process that has not looked yet. Deletion is explicit —
	// deleteStateEntry — and adoption of somebody else's deletion is
	// RefreshExternal's job, where the intent can be told from the staleness.
	for _, d := range disk {
		if !written[ident.Name(d.Name)] {
			out = append(out, d)
		}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(in.statePath, b, fsutil.FileMode); err != nil {
		return err
	}
	in.rememberStateMtime()
	return nil
}

// deleteStateEntry removes one project's entry from disk, explicitly. The
// counterpart to RemoveProject: a merge cannot express removal, so removal
// does not go through the merge.
func (in *Inbox) deleteStateEntry(name string) {
	if in.statePath == "" {
		return
	}
	err := fsutil.WithFileLock(in.statePath+".lock", lockWait, func() error {
		disk := readStateFile(in.statePath)
		kept := disk[:0]
		for _, d := range disk {
			if !ident.SameName(d.Name, name) {
				kept = append(kept, d)
			}
		}
		b, err := json.MarshalIndent(kept, "", "  ")
		if err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(in.statePath, b, fsutil.FileMode); err != nil {
			return err
		}
		in.rememberStateMtime()
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: state entry not removed: %v\n", err)
	}
}

// readStateFile parses state.json, tolerating absence and damage the way
// LoadState always has: unreadable means empty, and the next write replaces
// it rather than preserving a file this program cannot understand.
func readStateFile(path string) []Project {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var saved []Project
	if json.Unmarshal(b, &saved) != nil {
		return nil
	}
	return saved
}

// sameProject is the identity check LoadState uses: name alone is not enough,
// because the same name repointed at another directory is a different project
// and must not inherit this one's session.
func sameProject(a, b Project) bool {
	return a.Tool == b.Tool && ident.SameDir(a.Dir, b.Dir)
}

// adoptPersisted copies the durable fields from d onto p, leaving p's
// transient fields (git state, streaming text, activity) alone — those are
// ours by construction; disk never held them.
func adoptPersisted(p *Project, d Project) {
	p.SessionID = d.SessionID
	p.ForkFrom = d.ForkFrom
	p.Status = d.Status
	p.LastMessage = d.LastMessage
	p.LastErr = d.LastErr
	p.WaitReason = d.WaitReason
	p.WaitDetail = d.WaitDetail
	p.UpdatedAt = d.UpdatedAt
	p.History = d.History
	p.Trace = d.Trace
}

// rememberStateMtime records the mtime of the file this process just wrote,
// so RefreshExternal can tell its own writes from everybody else's.
func (in *Inbox) rememberStateMtime() {
	fi, err := os.Stat(in.statePath)
	if err != nil {
		return
	}
	in.mu.Lock()
	in.lastStateMtime = fi.ModTime()
	in.mu.Unlock()
}

// RefreshExternal adopts what other processes have written since we last
// looked: state entries (including projects added or removed elsewhere) and
// notes. Called from the UI's existing tick — one stat call when nothing
// changed, a read when something did.
//
// This is display freshness, not correctness. The merge makes every write
// safe regardless; this makes the dashboard show a harness-driven send while
// it runs instead of at next restart.
func (in *Inbox) RefreshExternal() {
	if in.statePath != "" {
		if fi, err := os.Stat(in.statePath); err == nil {
			in.mu.Lock()
			last := in.lastStateMtime
			in.mu.Unlock()
			if fi.ModTime().After(last) {
				in.adoptExternalState()
			}
		}
	}
	if in.notesPath != "" {
		if fi, err := os.Stat(in.notesPath); err == nil {
			in.mu.Lock()
			last := in.lastNotesMtime
			in.mu.Unlock()
			if fi.ModTime().After(last) {
				in.loadNotes()
				in.rememberNotesMtime()
			}
		}
	}
}

// adoptExternalState overlays the disk state onto memory for every project
// this process is not mid-turn on, and adopts membership changes: a project
// added by a harness appears here, one removed by a harness disappears.
func (in *Inbox) adoptExternalState() {
	disk := readStateFile(in.statePath)
	byName := make(map[string]Project, len(disk))
	for _, d := range disk {
		byName[ident.Name(d.Name)] = d
	}

	in.mu.Lock()
	// Adopt into existing projects first.
	kept := in.projects[:0]
	removed := []string{}
	for _, p := range in.projects {
		d, onDisk := byName[ident.Name(p.Name)]
		switch {
		case !onDisk && in.isKingLocked(p.Name):
			// A supervisor is provisioned, not stored: its absence from
			// state.json is the ordinary state of a fresh one, not a removal.
			kept = append(kept, p)
		case !onDisk:
			removed = append(removed, p.Name)
		case sameProject(d, *p):
			if _, live := in.active[p.Name]; !live {
				adoptPersisted(p, d)
				// An entry claiming to be mid-turn belongs to another
				// process. Whether that process still exists is what the
				// claim says — a Working status with no claim behind it is a
				// process that died, and showing it working forever is the
				// lie restart-time LoadState already refuses to tell.
				if p.Status == driver.StatusWorking && !in.claimHeldBy(p.Name) {
					p.Status = driver.StatusIdle
					p.WaitReason, p.WaitDetail = "", ""
				}
			}
			kept = append(kept, p)
		default:
			// Same name, different identity: the config this process loaded
			// still defines the project, so keep ours. The other writer's
			// entry loses to the config, which is the only shared definition
			// of what the fleet is.
			kept = append(kept, p)
		}
	}
	// Projects we do not know: adopted from their entry. Config is the
	// definition of the fleet, but the harness that added it wrote both, so
	// the state entry carries everything adoption needs.
	for key, d := range byName {
		if in.hasProjectLocked(key) {
			continue
		}
		if d.Status == driver.StatusWorking && !in.claimHeldBy(d.Name) {
			d.Status = driver.StatusIdle
			d.WaitReason, d.WaitDetail = "", ""
		}
		kept = append(kept, &d)
	}
	in.projects = kept
	in.mu.Unlock()

	for _, name := range removed {
		in.forgetProject(name)
	}
}

func (in *Inbox) hasProjectLocked(key string) bool {
	for _, p := range in.projects {
		if ident.Name(p.Name) == key {
			return true
		}
	}
	return false
}

// claimHeldBy reports whether a live claim exists for the project, whoever
// holds it. Working-with-no-claim is how a crashed process's last write would
// otherwise read forever.
func (in *Inbox) claimHeldBy(name string) bool {
	if in.claims == nil {
		return false
	}
	_, held := in.claims.Peek(ident.Name(name))
	return held
}

// notesRMW runs one read-modify-write cycle on the notes store under the
// cross-process lock: reload from disk, mutate, save. Every notes mutation on
// every side goes through here, which is what keeps a drop issued by one
// process from being resurrected by a save from another that never saw it.
//
// Session-only notes (no path) have nothing to serialise on and run fn
// directly. A lock that cannot be taken is reported and the operation
// proceeds anyway: dropping a note the user asked to drop, or a fact the
// supervisor took a turn to learn, costs more than the lost update this
// risks, and the risk needs two writers in the same ten seconds.
func (in *Inbox) notesRMW(fn func()) {
	if in.notesPath == "" {
		fn()
		return
	}
	err := fsutil.WithFileLock(in.notesPath+".lock", lockWait, func() error {
		in.loadNotes()
		fn()
		in.saveNotes()
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-inbox: notes lock: %v\n", err)
		fn()
		in.saveNotes()
	}
}

// rememberNotesMtime is rememberStateMtime for the notes store.
func (in *Inbox) rememberNotesMtime() {
	fi, err := os.Stat(in.notesPath)
	if err != nil {
		return
	}
	in.mu.Lock()
	in.lastNotesMtime = fi.ModTime()
	in.mu.Unlock()
}

// newTurnKey identifies one acquired claim across the supersede case: a turn
// cancelled and replaced must not release the claim its successor took.
func newTurnKey() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// acquireClaim takes the send claim, waiting out one special case: a claim
// held by this same process. A turn's handle resolves before the goroutine
// that filed it releases the claim — deliberately, since the turn is not
// fully filed until its save has run — so a follow-up send from this process
// can arrive a few milliseconds early. The subprocess behind such a claim
// has already exited (the handle said so), so waiting is correct. Two
// seconds is far beyond what the filing goroutine needs; a claim held by any
// other pid is a real conflict and fails immediately.
func (in *Inbox) acquireClaim(name, tool, key string) error {
	err := in.claims.Acquire(name, key, tool)
	var held *claim.HeldError
	if err == nil || !errors.As(err, &held) || held.Info.Pid != os.Getpid() {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		err = in.claims.Acquire(name, key, tool)
		if !errors.As(err, &held) || held.Info.Pid != os.Getpid() {
			return err
		}
	}
	return err
}

// SendAndWait sends one prompt to a project by name and waits for exactly
// that turn — the headless front-end's primitive. It resolves when the turn
// resolves: reply, error, or cancellation. A timeout bounds both the wait and
// the turn itself, so a caller that gives up also stops paying for the agent.
//
// ok is false when the turn could not be started or did not finish in bounds;
// the outcome carries the reason either way.
func (in *Inbox) SendAndWait(name, prompt string, timeout time.Duration) (TurnOutcome, bool) {
	wait := timeout
	if wait <= 0 {
		wait = configTurnTimeout(in)
		if wait <= 0 {
			wait = 30 * time.Minute
		}
	}
	// The resolve closure runs under the inbox mutex inside startSend, so it
	// must not take the lock itself; projectByName is documented caller-holds-mu.
	handle, err := in.startSendTimed(
		func() (*Project, error) { return in.projectByName(name) },
		prompt, prompt, true, timeout,
	)
	if err != nil {
		return TurnOutcome{Project: name, Err: err}, false
	}
	t := time.NewTimer(wait + 10*time.Second)
	defer t.Stop()
	select {
	case out := <-handle.Done:
		return out, true
	case <-t.C:
		return TurnOutcome{Project: name, Err: fmt.Errorf("turn did not finish within %s", wait)}, false
	case <-in.done:
		return TurnOutcome{Project: name, Err: errors.New("shutting down"), Cancelled: true}, false
	}
}

func configTurnTimeout(in *Inbox) time.Duration {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.turnTimeout
}

// HistoryOf returns a project's last n history messages, newest last.
func (in *Inbox) HistoryOf(name string, last int) ([]Message, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.projectByName(name)
	if err != nil {
		return nil, err
	}
	if last <= 0 || last > len(p.History) {
		last = len(p.History)
	}
	return append([]Message(nil), p.History[len(p.History)-last:]...), nil
}

// QueryGitByName is QueryGit for callers that hold a name rather than an
// index — every front-end but the dashboard's sidebar.
func (in *Inbox) QueryGitByName(name string, kind git.Kind) (string, error) {
	in.mu.Lock()
	_, err := in.projectByName(name)
	in.mu.Unlock()
	if err != nil {
		return "", err
	}
	return in.QueryGit(name, kind)
}
