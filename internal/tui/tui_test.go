package tui

import (
	"strings"
	"testing"
	"time"

	"agentinbox/internal/driver"
	"agentinbox/internal/inbox"
)

func TestAgeHuman(t *testing.T) {
	cases := []struct {
		d   time.Duration
		out string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{70 * time.Minute, "1h"},
		{2 * time.Hour, "2h"},
		{30 * time.Hour, "1d"},
		{7 * 24 * time.Hour, "7d"},
	}
	for _, c := range cases {
		if got := ageHuman(c.d); got != c.out {
			t.Errorf("ageHuman(%v) = %q; want %q", c.d, got, c.out)
		}
	}
}

func TestTruncateOneLine(t *testing.T) {
	cases := []struct {
		in, out string
		max     int
	}{
		{"hello", "hello", 60},
		{"line1\nline2", "line1 line2", 60},
		{"   padded   ", "padded", 60},
		{strings.Repeat("a", 100), strings.Repeat("a", 59) + "…", 60},
	}
	for _, c := range cases {
		got := truncateOneLine(c.in, c.max)
		if got != c.out {
			t.Errorf("truncateOneLine(%q, %d) = %q; want %q", c.in, c.max, got, c.out)
		}
	}
}

func TestRenderRowContainsFields(t *testing.T) {
	p := inbox.Project{
		Name:        "my-project",
		Tool:        "claude",
		Dir:         "/tmp",
		Status:      driver.StatusWaiting,
		LastMessage: "I need help with the foo",
	}
	row := renderRow(1, p, false)
	for _, want := range []string{"[1]", "my-project", "claude", "waiting", "I need help"} {
		if !strings.Contains(row, want) {
			t.Errorf("renderRow output missing %q; got %q", want, row)
		}
	}
}

func TestRenderRowSelectedDoesNotPanic(t *testing.T) {
	p := inbox.Project{Name: "x", Tool: "mock", Status: driver.StatusIdle}
	row := renderRow(2, p, true)
	if row == "" {
		t.Fatal("selected renderRow returned empty string")
	}
}

func TestStatusStyleAllKnownStatuses(t *testing.T) {
	// Just verify no panic across the status enum.
	for _, s := range []driver.Status{
		driver.StatusIdle,
		driver.StatusWorking,
		driver.StatusWaiting,
		driver.StatusError,
		driver.Status("unknown"),
	} {
		_ = statusStyle(s, string(s))
	}
}

func TestHelpTextMentionsKeys(t *testing.T) {
	h := helpText()
	for _, key := range []string{"navigate", "send", "view", "attach", "quit"} {
		if !strings.Contains(h, key) {
			t.Errorf("helpText missing %q", key)
		}
	}
}
