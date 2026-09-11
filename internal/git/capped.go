package git

import "unicode/utf8"

// cappedBuffer retains at most limit bytes while consuming everything
// written to it. Stopping reads at the cap would turn a memory bound into a
// subprocess deadlock — the child blocks writing a full pipe nobody drains —
// so excess bytes are read and discarded, and the answer says it was cut.
type cappedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &cappedBuffer{data: make([]byte, 0, limit), limit: limit}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - len(b.data)
	if len(p) > room {
		b.truncated = true
		p = p[:max(room, 0)]
	}
	b.data = append(b.data, p...)
	return n, nil // excess bytes are consumed, not stored
}

func (b *cappedBuffer) String() string {
	if !b.truncated {
		return string(b.data)
	}
	const marker = "\n[truncated]"
	if b.limit < len(marker) {
		return marker[:b.limit]
	}
	cut := b.limit - len(marker)
	for cut > 0 && !utf8.RuneStart(b.data[cut]) {
		cut--
	}
	return string(b.data[:cut]) + marker
}
