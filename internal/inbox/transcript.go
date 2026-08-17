package inbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// tailWindow is how much of the end of a transcript is read on the first
	// attempt. Big enough for a long final turn, small enough that a Stop hook
	// on a months-old session is not reading megabytes to answer one question.
	tailWindow = 256 * 1024
	// maxTailWindow bounds the search when the tail holds no assistant turn.
	maxTailWindow = 16 * 1024 * 1024
	// maxRecordLen skips a single JSONL record too large to be a text turn
	// rather than giving up on the file.
	maxRecordLen = 8 * 1024 * 1024
)

// LastAssistantText returns the concatenated text of the last assistant turn
// in a Claude Code transcript (JSONL). Schema verified against claude 2.1:
// lines are {type, message:{role, content:[{type:"text", text}]}}.
//
// The file is read backwards from the end in growing windows. Reading it
// forwards from byte zero meant every Stop event in a long-lived session cost
// a full scan of the whole history to find something near the end of it.
//
// An unreadable transcript is an error rather than an empty string. The caller
// files this text as the session's reply, and a read failure that looks like
// success files the wrong reply against the turn.
func LastAssistantText(transcriptPath string) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat transcript: %w", err)
	}
	size := info.Size()

	for window := int64(tailWindow); ; window *= 4 {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		start := size - window
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return "", fmt.Errorf("read transcript tail: %w", err)
		}
		// Unless we are at the very beginning of the file, the first line in
		// the window is almost certainly cut in half. Drop it — a later,
		// larger window will cover it whole.
		if start > 0 {
			if nl := bytes.IndexByte(buf, '\n'); nl >= 0 {
				buf = buf[nl+1:]
			} else {
				buf = nil
			}
		}
		if text, ok := lastAssistantIn(buf); ok {
			return text, nil
		}
		if window >= size || window >= maxTailWindow {
			// Read as far back as we are willing to go and found no assistant
			// turn. That is a legitimate answer for a transcript that has none.
			return "", nil
		}
	}
}

// lastAssistantIn scans a buffer of whole JSONL records backwards and returns
// the text of the last assistant turn in it.
func lastAssistantIn(buf []byte) (string, bool) {
	for len(buf) > 0 {
		// Walk back to the start of the final record in the buffer.
		end := len(buf)
		if buf[end-1] == '\n' {
			end--
		}
		if end == 0 {
			return "", false
		}
		start := bytes.LastIndexByte(buf[:end], '\n') + 1
		record := buf[start:end]
		buf = buf[:start]

		if len(record) > maxRecordLen {
			continue
		}
		if text, ok := assistantText(record); ok {
			return text, true
		}
	}
	return "", false
}

// assistantText decodes one JSONL record, reporting the turn's text when the
// record is an assistant turn that actually said something.
func assistantText(record []byte) (string, bool) {
	record = bytes.TrimSpace(record)
	if len(record) == 0 || record[0] != '{' {
		return "", false
	}
	var d struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(record, &d) != nil || d.Type != "assistant" {
		return "", false
	}
	var sb strings.Builder
	for _, c := range d.Message.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	t := strings.TrimSpace(sb.String())
	return t, t != ""
}
