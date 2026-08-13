package inbox

import (
	"sync"
	"time"

	"github.com/im-tyler/agent-inbox/internal/git"
	"github.com/im-tyler/agent-inbox/internal/ident"
	"github.com/im-tyler/agent-inbox/internal/usage"
)

// Reading the fleet's working trees.
//
// This is a poll rather than a watch, and the interval is the whole design
// question. The dashboard already ticks once a second; refreshing git on that
// tick would be one subprocess per project per second, which on a fleet of a
// dozen is a fork storm in service of a sidebar line that changes a few times
// an hour.
//
// So: a slow interval for the ambient case, plus a nudge for the moment the
// answer is actually expected to have changed — an agent turn finishing, which
// is the only thing this program does that makes a project's tree move.

const (
	// DefaultGitRefresh is how often trees are re-read when nothing has
	// happened.
	DefaultGitRefresh = 10 * time.Second

	// gitConcurrency bounds how many git processes exist at once. The point is
	// not speed — a status takes milliseconds — it is that a fleet of thirty
	// should not fork thirty processes in the same instant.
	gitConcurrency = 4
)

// WithGitRefresh starts reading the fleet's working trees on an interval.
// A non-positive interval leaves the feature off, which is what tests and the
// headless paths want: nothing here is required for the inbox to work.
func (in *Inbox) WithGitRefresh(every time.Duration) *Inbox {
	if every <= 0 {
		return in
	}
	in.track(func() { in.refreshLoop(every) })
	return in
}

// WithUsage attaches a source for how much has been spent against the rate
// limit. Optional: with none, the capacity line is absent everywhere rather
// than showing zero, because "no source" and "no usage" are opposite facts
// that render as the same number.
func (in *Inbox) WithUsage(src usage.Source) *Inbox {
	in.mu.Lock()
	in.usageSrc = src
	in.mu.Unlock()
	return in
}

func (in *Inbox) refreshLoop(every time.Duration) {
	// Once immediately, so the first frame the user sees already has branches
	// in it rather than filling them in a beat later.
	in.RefreshGit()
	in.refreshUsage()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-in.done:
			return
		case <-in.gitNudge:
			in.RefreshGit()
		case <-t.C:
			in.RefreshGit()
			in.refreshUsage()
		}
	}
}

// RefreshUsage re-reads the usage source now, rather than waiting for the
// next tick.
func (in *Inbox) RefreshUsage() { in.refreshUsage() }

// refreshUsage re-reads the usage source. The first read of a long history
// takes a second or so and every one after it is a few milliseconds, which is
// why this is on a timer and not on the render path.
func (in *Inbox) refreshUsage() {
	in.mu.Lock()
	src := in.usageSrc
	in.mu.Unlock()
	if src == nil {
		return
	}
	snap, err := src.Read()
	in.mu.Lock()
	if err != nil {
		// Keep the last good snapshot. A transient read failure should not
		// blank a figure that was correct a moment ago.
		in.usageErr = err
		in.mu.Unlock()
		return
	}
	in.usageSnap, in.usageErr = snap, nil
	in.mu.Unlock()
}

// Usage is the last-read capacity snapshot. The zero value means no source is
// configured or none has been read yet, and Snapshot.Summary renders that as
// nothing.
func (in *Inbox) Usage() usage.Snapshot {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.usageSnap
}

// nudgeGit asks for a refresh at the next opportunity, without blocking. A
// full buffer means one is already pending, and two turns finishing together
// should produce one refresh, not two.
func (in *Inbox) nudgeGit() {
	select {
	case in.gitNudge <- struct{}{}:
	default:
	}
}

// RefreshGit re-reads every project's working tree and files the result.
//
// The mutex is never held across a subprocess. Reading a dozen trees takes long
// enough that holding it would stall every send, every snapshot and every
// render for the duration — so the targets are copied out, git runs unlocked,
// and the results are written back one at a time.
func (in *Inbox) RefreshGit() {
	type target struct{ name, dir string }

	in.mu.Lock()
	targets := make([]target, 0, len(in.projects))
	for _, p := range in.projects {
		targets = append(targets, target{p.Name, p.Dir})
	}
	in.mu.Unlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, gitConcurrency)
	for _, t := range targets {
		if in.closed() {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-in.done:
				return
			}
			st := git.Inspect(in.bgCtx, t.dir)

			in.mu.Lock()
			defer in.mu.Unlock()
			// Re-resolve by name. A project can be removed, or repointed at a
			// different directory, while its git call was in flight — and a
			// state read from the old directory filed against the new one is a
			// branch name from a repository this project is no longer in.
			p, err := in.projectByName(t.name)
			if err != nil || !ident.SameDir(p.Dir, t.dir) {
				return
			}
			p.Git = st
		}()
	}
	wg.Wait()
}

// GitStateOf is a project's last-read working tree, by name.
func (in *Inbox) GitStateOf(name string) git.State {
	in.mu.Lock()
	defer in.mu.Unlock()
	p, err := in.projectByName(name)
	if err != nil {
		return git.State{Err: git.ErrNotARepo}
	}
	return p.Git
}

// QueryGit answers one of the fixed read-only questions about a project's tree.
//
// This is the cheap path the supervisor has been missing. Everything else it
// can learn about a project costs an agent turn in that project's session; this
// costs a subprocess, and returns the same answer every time.
func (in *Inbox) QueryGit(name string, kind git.Kind) (string, error) {
	in.mu.Lock()
	p, err := in.projectByName(name)
	if err != nil {
		in.mu.Unlock()
		return "", err
	}
	dir := p.Dir
	in.mu.Unlock()
	return git.Query(in.bgCtx, dir, kind)
}
