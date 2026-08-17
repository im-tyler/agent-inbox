package main

import (
	"testing"

	"github.com/im-tyler/agent-inbox/internal/inbox"
)

// Claude sends the Notification hook for two different situations behind one
// message field. Only one of them is a state change.
func TestClassifyNotification(t *testing.T) {
	cases := []struct {
		msg        string
		wantReason inbox.Reason
	}{
		{"Claude needs your permission to use Bash", inbox.ReasonPermission},
		{"Permission required to edit /etc/hosts", inbox.ReasonPermission},
		{"Claude is waiting for your input", inbox.ReasonQuestion},
		// The sixty-second idle nudge. Filing it would flip a working project
		// to "needs attention" for the crime of the user not typing.
		{"Claude is waiting", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, detail := classifyNotification(c.msg)
		if got != c.wantReason {
			t.Errorf("classifyNotification(%q) = %q, want %q", c.msg, got, c.wantReason)
		}
		if got != "" && detail == "" {
			t.Errorf("classifyNotification(%q) kept no detail; the specific ask is the useful half", c.msg)
		}
	}
}
