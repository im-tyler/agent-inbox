package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	disk, derr := in.diskOrQuarantine()
	if derr != nil {
		return derr
	}
	diskByName := make(map[string]Project, len(disk))
	for _, d := range disk {
		diskByName[ident.Name(d.Name)] = d
	}

	removed := readRemoved(in.statePath)
	out := make([]Project, 0, len(ours)+len(disk))
	written := make(map[string]bool, len(ours))
	for i := range ours {
		p := ours[i]
		key := ident.Name(p.Name)
		// A tombstoned project was deliberately removed; this memory is stale.
		// Writing the entry back is the resurrection the tombstone exists to
		// prevent (F14).
		if _, gone := removed[key]; gone {
			continue
		}
		written[key] = true
		if d, ok := diskByName[key]; ok && !localTurn[key] && sameProject(d, p) && d.UpdatedAt.After(p.UpdatedAt) {
			adoptPersisted(&p, d)
		}
		out = append(out, p)
	}
	// Entries only the other process knows about are kept, not dropped: a
	// merge must never delete, because "absent from our memory" is also the
	// state of a process that has not looked yet. Deletion is explicit —
	// deleteStateEntry plus a tombstone — and adoption of somebody else's
	// deletion is RefreshExternal's job, where the intent can be told from
	// the staleness.
	for _, d := range disk {
		key := ident.Name(d.Name)
		if written[key] {
			continue
		}
		if _, gone := removed[key]; gone {
			continue // a resurrected entry, dropped again
		}
		out = append(out, d)
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

// diskOrQuarantine reads the state store for a write path. Damage is
// quarantined, not written over: the evidence survives beside the live path
// and the next save starts fresh from memory. Writing a merge (or a removal)
// on top of an unreadable store is the "repair" that erased it (F17).
func (in *Inbox) diskOrQuarantine() ([]Project, error) {
	disk, derr := readStateFile(in.statePath)
	if derr == nil {
		return disk, nil
	}
	if q := quarantineState(in.statePath); q != "" {
		return nil, fmt.Errorf("state file was damaged and is quarantined at %s; retry the operation to start a fresh store: %w", q, derr)
	}
	return nil, fmt.Errorf("state file damaged and could not be quarantined: %w", derr)
}

// Deletion tombstones (F14).
//
// A merge cannot express removal, and a merge must never delete — so a
// removed project's state entry is gone but not forgotten: a second inbox
// still holding the project in memory would append it right back on its next
// save, and every other frontend then adopted the resurrection. The
// tombstone store names what was deliberately removed, so a stale writer's
// merge drops what it does not know was deleted, and adoption of disk-only
// entries can tell "added elsewhere" from "removed elsewhere and resurrected".
// Re-adding the project clears its tombstone: an explicit add is an explicit
// add.

// removedPath is the tombstone store beside state.json.
func removedPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "removed.json")
}

// maxTombstones bounds the store. Deletions are rare; the set only has to
// outlive the staleness of every live process, not history.
const maxTombstones = 128

type removedEntry struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func readRemoved(statePath string) map[string]time.Time {
	out := map[string]time.Time{}
	if statePath == "" {
		return out
	}
	b, err := os.ReadFile(removedPath(statePath))
	if err != nil {
		return out
	}
	var entries []removedEntry
	if json.Unmarshal(b, &entries) != nil {
		return out
	}
	for _, e := range entries {
		out[ident.Name(e.Name)] = e.At
	}
	return out
}

func writeRemoved(statePath string, m map[string]time.Time) {
	entries := make([]removedEntry, 0, len(m))
	for name, at := range m {
		entries = append(entries, removedEntry{Name: name, At: at})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	if len(entries) > maxTombstones {
		entries = entries[len(entries)-maxTombstones:]
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = fsutil.WriteFileAtomic(removedPath(statePath), b, fsutil.FileMode)
}

// tombstoneProject records a deletion durably.
func tombstoneProject(statePath, name string) {
	if statePath == "" {
		return
	}
	_ = fsutil.WithFileLock(removedPath(statePath)+".lock", lockWait, func() error {
		m := readRemoved(statePath)
		m[ident.Name(name)] = time.Now()
		writeRemoved(statePath, m)
		return nil
	})
}

// untombstoneProject clears a deletion so an explicit re-add sticks.
func untombstoneProject(statePath, name string) {
	if statePath == "" {
		return
	}
	_ = fsutil.WithFileLock(removedPath(statePath)+".lock", lockWait, func() error {
		m := readRemoved(statePath)
		if _, gone := m[ident.Name(name)]; !gone {
			return nil
		}
		delete(m, ident.Name(name))
		writeRemoved(statePath, m)
		return nil
	})
}

// deleteStateEntry removes one project's entry from disk, explicitly. The
// counterpart to RemoveProject: a merge cannot express removal, so removal
// does not go through the merge.
func (in *Inbox) deleteStateEntry(name string) {
	if in.statePath == "" {
		return
	}
	err := fsutil.WithFileLock(in.statePath+".lock", lockWait, func() error {
		disk, derr := in.diskOrQuarantine()
		if derr != nil {
			return derr
		}
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

// readStateFile parses state.json, distinguishing a store that does not exist
// yet (normal, empty) from one that exists but cannot be read or parsed
// (damage). Mapping damage to an empty slice made every absent project look
// intentionally deleted, and the refresh path then destroyed notes and
// membership over a file nobody had actually read (F17).
func readStateFile(path string) ([]Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var saved []Project
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return saved, nil
}

// quarantineState moves a damaged state file aside, preserving the evidence
// before any fresh write replaces it. The renamed copy keeps what can be
// inspected; the live path becomes absent, so the next save starts a fresh
// store from current memory instead of failing forever.
func quarantineState(path string) string {
	q := fmt.Sprintf("%s.bad-%d", path, time.Now().UnixNano())
	if os.Rename(path, q) != nil {
		return ""
	}
	return q
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
	p.Revision = d.Revision
	p.History = d.History
	p.Trace = d.Trace
	// Union the dedup sets: an event this process applied and one the other
	// process applied must both count as applied after adoption, or the
	// loser's spool file double-applies on its next ingest.
	p.SeenEvents = unionSeenEvents(p.SeenEvents, d.SeenEvents)
}

// unionSeenEvents merges two bounded dedup sets, newest last.
func unionSeenEvents(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, s := range b {
		if !containsString(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > maxSeenEvents {
		out = out[len(out)-maxSeenEvents:]
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
//
// A store that cannot be read is not a fleet that shrank to nothing: damage
// must leave memory alone and surface, not delete notes and membership over
// a file nobody actually read (F17).
func (in *Inbox) adoptExternalState() {
	disk, derr := readStateFile(in.statePath)
	if derr != nil {
		in.mu.Lock()
		in.saveErr = fmt.Errorf("state not refreshed: %v", derr)
		in.mu.Unlock()
		fmt.Fprintf(os.Stderr, "agent-inbox: state file unreadable, keeping current view: %v\n", derr)
		return
	}
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
	// the state entry carries everything adoption needs. A tombstoned name
	// is not an addition — it is a removal somebody's stale write
	// resurrected, and adopting it would undo the deletion everywhere.
	tombstones := readRemoved(in.statePath)
	for key, d := range byName {
		if in.hasProjectLocked(key) {
			continue
		}
		if _, gone := tombstones[key]; gone {
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
// directly. A lock that cannot be taken fails the operation: proceeding
// unlocked committed a stale in-memory snapshot over whatever the lock holder
// was writing — the exact lost update the lock exists to prevent (F18). The
// caller's return value then reports that nothing happened, which the user
// can retry; a dropped note is recoverable, a clobbered store is not.
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
		fmt.Fprintf(os.Stderr, "agent-inbox: notes change skipped, store is busy or unreadable: %v\n", err)
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

// refreshOwnedProject re-reads the persisted entry for name while the caller
// holds the send claim, so a send runs against the session the last turn left
// behind — not the snapshot this process happened to load who-knows-when.
//
// Winning the claim means nobody else is mid-turn on the project, so a disk
// Working status is stale by definition: it is a writer that died before
// filing an outcome, and adopting it verbatim would make our own second-pass
// Working check refuse a send we own.
func (in *Inbox) refreshOwnedProject(name string) {
	if in.statePath == "" {
		return
	}
	disk, derr := readStateFile(in.statePath)
	if derr != nil {
		// Damaged store: the claim is ours, so memory is the best available
		// answer, and a failed read must not become a session reset.
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.projectByName(name)
	if err != nil {
		return
	}
	if _, live := in.active[p.Name]; live {
		return
	}
	for _, d := range disk {
		if !ident.SameName(d.Name, name) || !sameProject(d, *p) || !d.UpdatedAt.After(p.UpdatedAt) {
			continue
		}
		adoptPersisted(p, d)
		if p.Status == driver.StatusWorking {
			p.Status = driver.StatusIdle
			p.WaitReason, p.WaitDetail = "", ""
		}
		break
	}
}

// SendAndWait sends one prompt to a project by name and waits for exactly
// that turn — the headless front-end's primitive. It resolves when the turn
// resolves: reply, error, or cancellation. A timeout bounds both the wait and
// the turn itself, so a caller that gives up also stops paying for the agent.
//
// ok is false when the turn could not be started or did not finish in bounds;
// the outcome carries the reason either way.
func (in *Inbox) SendAndWait(name, prompt string, timeout time.Duration) (TurnOutcome, bool) {
	return in.SendAndWaitCtx(context.Background(), name, prompt, timeout)
}

// SendAndWaitCtx is SendAndWait bound to a caller's context — an MCP request,
// a cancelled CLI pipe. Cancelling ctx cancels the turn itself (the child
// dies with the group the driver owns), not just this process's wait for it,
// so a disconnect does not leave the agent spending in the background (F11).
// The outcome reports the cancellation; ok is false.
func (in *Inbox) SendAndWaitCtx(ctx context.Context, name, prompt string, timeout time.Duration) (TurnOutcome, bool) {
	wait := timeout
	if wait <= 0 {
		wait = configTurnTimeout(in)
		if wait <= 0 {
			wait = 30 * time.Minute
		}
	}
	// The resolve closure runs under the inbox mutex inside startSend, so it
	// must not take the lock itself; projectByName is documented caller-holds-mu.
	handle, err := in.startSendTimedCtx(ctx,
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
	case <-ctx.Done():
		// Cancel the specific turn. The cancels entry may already belong to
		// a successor that started between our turn ending and this firing,
		// so only cancel when the active turn is still ours — cancelling by
		// project name alone could kill a turn this request never started.
		in.mu.Lock()
		cancel, ok := in.cancels[name]
		if cur, live := in.active[name]; !live || cur.ID() != handle.ID {
			ok = false
		} else {
			delete(in.cancels, name)
		}
		in.mu.Unlock()
		if ok {
			cancel()
		}
		// Wait out the owned-process cleanup so the child is reaped before
		// returning, but never past the caller's own deadline.
		select {
		case out := <-handle.Done:
			out.Project = name
			out.Cancelled = true
			if out.Err == nil {
				out.Err = ctx.Err()
			}
			return out, false
		case <-t.C:
			return TurnOutcome{Project: name, Err: fmt.Errorf("turn did not finish within %s", wait), Cancelled: true}, false
		case <-in.done:
			return TurnOutcome{Project: name, Err: errors.New("shutting down"), Cancelled: true}, false
		}
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
