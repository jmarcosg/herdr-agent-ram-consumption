package model

import "testing"

func TestPretty(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-opus-5-5", "Opus 5.5"},
		{"claude-opus-5-5[1m]", "Opus 5.5"},
		{"claude-sonnet-4-5-20250929", "Sonnet 4.5"},
		{"claude-sonnet-4-20250514", "Sonnet 4"},
		{"claude-opus-4-0", "Opus 4"},
		{"claude-3-5-sonnet-20241022", "Sonnet 3.5"},
		{"anthropic.claude-haiku-4-5@20251001", "Haiku 4.5"},
		{"vertex/claude-opus-4-1", "Opus 4.1"},
		{"gpt-5-codex", "gpt-5-codex"},
		{"", ""},
		// from Cornercase
		{"claude-haiku-4-5-20251001", "Haiku 4.5"},
		{"claude-fable-5-1", "Fable 5.1"},
		{"us.anthropic.claude-opus-4-1-20250805-v1:0", "Opus 4.1"},
		{"claude-opus-4-1@20250805", "Opus 4.1"},
		{"glm-4.6", "glm-4.6"},
	}
	for _, c := range cases {
		if got := Pretty(c.in); got != c.want {
			t.Errorf("Pretty(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
