package tui

import (
	"strings"
	"testing"
	"time"
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

// (renderRow tests removed with the list view they exercised — the sidebar
// rows the main view actually draws are covered by the sidebar tests.)

// (statusStyle test removed — function was dead code, now deleted.)

// The overlay has to describe the view you are in when you open it. It used
// to document index selection and a ":" menu belonging to a screen the program
// could no longer reach.
func TestHelpTextDescribesTheMainView(t *testing.T) {
	h := helpText()
	for _, want := range []string{"chat focused", "fleet focused", "tab", "send", "attach", "quit"} {
		if !strings.Contains(h, want) {
			t.Errorf("helpText missing %q", want)
		}
	}
	for _, gone := range []string{"more actions", "select by index", "king mode"} {
		if strings.Contains(h, gone) {
			t.Errorf("helpText still documents the removed list view: %q", gone)
		}
	}
}
