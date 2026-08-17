package driver

import (
	"encoding/json"
	"strings"
	"testing"
)

// A text part is re-sent whole as it grows, so emitting what arrives would
// repeat everything said so far on every event.
func TestTextAccumulatorEmitsOnlyTheNewText(t *testing.T) {
	a := newTextAccumulator()
	if got := a.add("p1", "Hel"); got != "Hel" {
		t.Errorf("first delta = %q, want Hel", got)
	}
	if got := a.add("p1", "Hello"); got != "lo" {
		t.Errorf("second delta = %q, want lo", got)
	}
	if got := a.add("p1", "Hello there"); got != " there" {
		t.Errorf("third delta = %q, want ' there'", got)
	}
	if got := a.text(); got != "Hello there" {
		t.Errorf("text() = %q", got)
	}
}

// Several parts concatenate in arrival order — that is the reply.
func TestTextAccumulatorConcatenatesPartsInOrder(t *testing.T) {
	a := newTextAccumulator()
	a.add("p1", "first ")
	a.add("p2", "second")
	a.add("p1", "first ") // a repeat contributes nothing
	if got := a.text(); got != "first second" {
		t.Errorf("text() = %q, want 'first second'", got)
	}
}

// A rewritten part is re-sent whole rather than diffed, because a bogus delta
// would corrupt what the user is watching arrive.
func TestTextAccumulatorHandlesARewrittenPart(t *testing.T) {
	a := newTextAccumulator()
	a.add("p1", "abcdef")
	if got := a.add("p1", "xyz"); got != "xyz" {
		t.Errorf("delta = %q, want the whole rewritten text", got)
	}
	if got := a.text(); got != "xyz" {
		t.Errorf("text() = %q, want xyz", got)
	}
}

// Parts with no id still contribute. Dropping them would lose the reply.
func TestTextAccumulatorKeepsAnonymousParts(t *testing.T) {
	a := newTextAccumulator()
	a.add("", "one ")
	a.add("", "two")
	if got := a.text(); got != "one two" {
		t.Errorf("text() = %q, want 'one two'", got)
	}
}

// OpenCode is the only driver that was not a StreamingDriver, so its projects
// showed a bare "working" for the whole turn while claude and codex showed
// live tool calls.
func TestOpenCodeIsAStreamingDriver(t *testing.T) {
	var d Driver = NewOpenCode("", false)
	if _, ok := d.(StreamingDriver); !ok {
		t.Error("OpenCode does not implement StreamingDriver")
	}
}

// The event vocabulary this driver reads, captured from opencode 1.18.18. If
// upstream renames one of these the parse goes quiet rather than loud, so the
// shape is pinned here.
func TestOpenCodeEventShapeIsUnderstood(t *testing.T) {
	lines := []string{
		`{"type":"step_start","sessionID":"ses_1","part":{"id":"prt_0","type":"step-start"}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"type":"tool","tool":"bash","state":{"status":"completed","title":"echo hi"}}}`,
		`{"type":"text","sessionID":"ses_1","part":{"id":"prt_1","type":"text","text":"hello"}}`,
		`{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop"}}`,
	}
	acc := newTextAccumulator()
	var kinds []string
	var session string
	sawStop := false
	for _, ln := range lines {
		ev := decodeEvent(t, ln)
		if ev.SessionID != "" {
			session = ev.SessionID
		}
		kinds = append(kinds, ev.Type)
		switch ev.Type {
		case "text":
			acc.add(ev.Part.ID, ev.Part.Text)
		case "tool_use":
			if ev.Part.Tool != "bash" {
				t.Errorf("tool = %q, want bash", ev.Part.Tool)
			}
		case "step_finish":
			if ev.Part.Reason == "stop" {
				sawStop = true
			}
		}
	}
	if session != "ses_1" {
		t.Errorf("session = %q — every event carries it, and it is what makes the id reliable", session)
	}
	if acc.text() != "hello" {
		t.Errorf("reply = %q, want hello", acc.text())
	}
	if !sawStop {
		t.Error("the terminal step_finish was not recognised")
	}
	if strings.Join(kinds, ",") != "step_start,tool_use,text,step_finish" {
		t.Errorf("event order = %v", kinds)
	}
}

// "tool-calls" ends a step so another can begin; only "stop" ends the turn.
// Treating either as terminal cuts every tool-using turn off at its first tool.
func TestIntermediateStepFinishIsNotTheEnd(t *testing.T) {
	ev := decodeEvent(t, `{"type":"step_finish","sessionID":"s","part":{"reason":"tool-calls"}}`)
	if ev.Part.Reason == "stop" {
		t.Fatal("an intermediate step was read as the end of the turn")
	}
}

func decodeEvent(t *testing.T, line string) ocEvent {
	t.Helper()
	var ev ocEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return ev
}
