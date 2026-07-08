package inbox

import "testing"

func TestParseKingDirectives(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []KingDirective
	}{
		{
			name:  "single directive",
			input: "I'll check on that.\n[send to maccel: run the tests]",
			expected: []KingDirective{
				{Target: "maccel", Message: "run the tests"},
			},
		},
		{
			name:  "multiple directives",
			input: "[send to maccel: write tests]\n[send to haven: review the plan]",
			expected: []KingDirective{
				{Target: "maccel", Message: "write tests"},
				{Target: "haven", Message: "review the plan"},
			},
		},
		{
			name:     "no directives",
			input:    "Everything looks good, no action needed.",
			expected: nil,
		},
		{
			name:  "case insensitive send to",
			input: "[SEND TO maccel: do the thing]",
			expected: []KingDirective{
				{Target: "maccel", Message: "do the thing"},
			},
		},
		{
			name:  "empty message skipped",
			input: "[send to maccel: ]",
			expected: nil,
		},
		{
			name:  "missing colon skipped",
			input: "[send to maccel]",
			expected: nil,
		},
		{
			name:  "message with colons",
			input: "[send to maccel: run: go test ./...]",
			expected: []KingDirective{
				{Target: "maccel", Message: "run: go test ./..."},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseKingDirectives(c.input)
			if len(got) != len(c.expected) {
				t.Fatalf("got %d directives, want %d: %v", len(got), len(c.expected), got)
			}
			for i, d := range got {
				if d.Target != c.expected[i].Target || d.Message != c.expected[i].Message {
					t.Errorf("directive %d: got {%s, %q}, want {%s, %q}",
						i, d.Target, d.Message, c.expected[i].Target, c.expected[i].Message)
				}
			}
		})
	}
}

func TestTruncateForKing(t *testing.T) {
	cases := []struct {
		input string
		max   int
		want  string
	}{
		{"short", 10, "short"},
		{"line one\nline two", 20, "line one line two"},
		{"  spaced  ", 20, "spaced"},
		{makeLongString(100), 50, makeLongString(49) + "…"},
	}
	for _, c := range cases {
		got := truncateForKing(c.input, c.max)
		if got != c.want {
			t.Errorf("truncateForKing(%q, %d) = %q, want %q", c.input, c.max, got, c.want)
		}
	}
}

func makeLongString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
