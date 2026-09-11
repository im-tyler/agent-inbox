package driver

import (
	"bufio"
	"io"
)

// jsonlReader reads newline-delimited JSON from a subprocess, and keeps
// reading past a line too large to hand back.
//
// bufio.Scanner cannot do that. Given a line longer than its buffer it stops,
// and the code that used it then called cmd.Wait with the pipe undrained: the
// child blocked writing the rest of that line, the parent blocked waiting for
// the child, and the turn sat there until the context deadline killed it. A
// successful model turn was reported as a timeout, and the trigger was simply
// one tool result larger than a megabyte — routine for a build log or a large
// file read.
//
// Skipping an oversized non-terminal event is fine. Stopping the read is not.
type jsonlReader struct {
	br    *bufio.Reader
	limit int
	// Skipped counts lines dropped for exceeding the limit, so a caller can
	// say that something was omitted rather than implying it never existed.
	Skipped int
}

// maxJSONLine bounds one event. Tool output is the reason it is generous;
// beyond this the event is skipped and the stream keeps moving.
const maxJSONLine = 16 << 20

func newJSONLReader(r io.Reader, limit int) *jsonlReader {
	if limit <= 0 {
		limit = maxJSONLine
	}
	// A large starting buffer keeps the common case to a single read.
	return &jsonlReader{br: bufio.NewReaderSize(r, 64<<10), limit: limit}
}

// Next returns the next line, or io.EOF when the stream ends. Lines longer
// than the limit are skipped — consumed to their newline and counted — rather
// than ending the read.
func (j *jsonlReader) Next() ([]byte, error) {
	for {
		line, err := j.readLine()
		if err != nil {
			return nil, err
		}
		if line == nil {
			continue // oversized, already drained
		}
		return line, nil
	}
}

// readLine returns one whole line, nil if it was too long to keep.
//
// A final chunk can arrive together with io.EOF — ReadSlice returns both the
// bytes it read and the error — and those bytes are the tail of the last
// record. Dropping them (the error branch ignoring chunk) lost a short final
// record outright and truncated a long one; accounting for chunk first, and
// applying the limit to buf plus chunk, returns the complete final record
// once and io.EOF on the next call.
func (j *jsonlReader) readLine() ([]byte, error) {
	var buf []byte
	overLimit := false
	for {
		chunk, err := j.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Partial line. Keep it only while it still fits.
			if !overLimit {
				if len(buf)+len(chunk) <= j.limit {
					buf = append(buf, chunk...)
				} else {
					overLimit = true
					j.Skipped++
					buf = nil
				}
			}
			continue
		}
		if !overLimit {
			if len(buf)+len(chunk) > j.limit {
				overLimit = true
				j.Skipped++
				buf = nil
			} else if len(chunk) > 0 {
				buf = append(buf, chunk...)
			}
		}
		if err != nil {
			if len(buf) > 0 && !overLimit {
				return buf, nil // last line, unterminated
			}
			return nil, err
		}
		if overLimit {
			return nil, nil
		}
		return buf, nil
	}
}
