// Package logbook is the king's operating memory: what was decided, what was
// left open, why — the thread a supervisor would otherwise lose the moment
// its session ends.
//
// It is deliberately not the notes store. Notes are facts about the fleet,
// curated, bounded, injected; the logbook is the record of supervising
// itself, append-only, read on demand. A fact like "teploy depends on
// neutron's client" belongs in notes, where every king sees it forever; "asked
// neutron to finish the client by Friday, teploy is parked until then" is a
// logbook entry — true this week, noise next month, and the first thing the
// next king session needs to read.
package logbook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-tyler/agent-inbox/internal/fsutil"
)

// maxEntryLen bounds one entry. A log entry is a decision and its context,
// not a transcript; replies belong to the projects that produced them.
const maxEntryLen = 2000

// Entry is one thing a king recorded.
type Entry struct {
	T      time.Time `json:"t"`
	Author string    `json:"author"`
	Text   string    `json:"text"`
}

// Path is the logbook's location under the data dir.
func Path(dataDir string) string { return filepath.Join(dataDir, "logbook.jsonl") }

// lockWait matches the wait every other cross-process lock in this program
// bounds itself by.
const lockWait = 10 * time.Second

// Append records one entry. The append is serialised across processes with
// the same file lock everything else uses: two kings closing down at once
// must not interleave their last words mid-line.
func Append(path, author, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) > maxEntryLen {
		text = text[:maxEntryLen] + "\n[truncated]"
	}
	if author == "" {
		author = "unknown"
	}
	b, err := json.Marshal(Entry{T: time.Now(), Author: author, Text: text})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return fsutil.WithFileLock(path+".lock", lockWait, func() error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fsutil.FileMode)
		if err != nil {
			return err
		}
		defer f.Close()
		// A process killed mid-append can leave a partial line with no
		// newline. Writing after it would glue the new entry onto the
		// fragment — one line, unparseable, and the new entry lost with the
		// old one. Repair the boundary first: what crashed stays
		// unreadable, but what comes after it survives.
		if err := ensureNewlineTerminated(f); err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			return err
		}
		return f.Sync()
	})
}

// ensureNewlineTerminated appends a newline when the file does not end with
// one, positioning the write offset at the end. An empty or new file needs
// nothing.
func ensureNewlineTerminated(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		_, err := f.Seek(0, io.SeekEnd)
		return err
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err != nil {
		return err
	}
	_, err = f.Seek(0, io.SeekEnd)
	if err != nil || buf[0] == '\n' {
		return err
	}
	_, err = f.Write([]byte("\n"))
	return err
}

// Read returns the last n entries, oldest first. A missing logbook is an
// empty one — the first session of a new install should read silence, not an
// error. A damaged line is skipped rather than fatal: the log predates
// nothing, but one truncated write must not cost the rest of the history.
func Read(path string, last int) []Entry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []Entry
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		entries = append(entries, e)
	}
	if last > 0 && len(entries) > last {
		entries = entries[len(entries)-last:]
	}
	return entries
}
