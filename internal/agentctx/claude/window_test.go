package claude

import "testing"

func TestWindow(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		tokens    int
		env       map[string]string
		requested string
		want      int
	}{
		{"opus 5.5 native long", "claude-opus-5-5", 100_000, nil, "", 1_000_000},
		{"opus 4.7 native long", "claude-opus-4-7", 1000, nil, "", 1_000_000},
		{"sonnet 5", "claude-sonnet-5", 1000, nil, "", 1_000_000},
		{"fable", "claude-fable-5-1", 1000, nil, "", 1_000_000},
		{"opus 4.6 standard", "claude-opus-4-6", 1000, nil, "", 200_000},
		{"sonnet 4.5 under 200k", "claude-sonnet-4-5-20250929", 150_000, nil, "", 200_000},
		{"sonnet 4.5 over 200k", "claude-sonnet-4-5-20250929", 300_000, nil, "", 1_000_000},
		{"requested alias 1m", "claude-sonnet-4-5-20250929", 50_000, nil, "sonnet[1m]", 1_000_000},
		{"requested id 1M", "claude-sonnet-4-5-20250929", 1000, nil, "claude-sonnet-4-5[1M]", 1_000_000},
		{"requested other model", "claude-haiku-4-5-20251001", 1000, nil, "sonnet[1m]", 200_000},
		{"plain request", "claude-sonnet-4-5-20250929", 1000, nil, "sonnet", 200_000},
		{"disable 1m", "claude-opus-5-5", 1000, map[string]string{"CLAUDE_CODE_DISABLE_1M_CONTEXT": "1"}, "", 200_000},
		{"disable 1m beats request", "claude-opus-5-5", 1000, map[string]string{"CLAUDE_CODE_DISABLE_1M_CONTEXT": "true"}, "opus[1m]", 200_000},
		{"max tokens, unknown model", "glm-4.6", 1000, map[string]string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "500000"}, "", 500_000},
		{"max tokens ignored for claude", "claude-sonnet-4-5", 1000, map[string]string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "128000"}, "", 200_000},
		{"max tokens with compacting off", "claude-opus-5-5", 1000, map[string]string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS": "128000", "DISABLE_COMPACT": "1"}, "", 128_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Window(tt.model, tt.tokens, tt.env, tt.requested); got != tt.want {
				t.Errorf("Window = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct{ tokens, window, want int }{
		{149_954, 200_000, 75},
		{1_000, 200_000, 1},
		{400, 200_000, 0},
		{165_611, 1_000_000, 17},
		{250_000, 200_000, 100},
	}
	for _, tt := range tests {
		if got := percent(tt.tokens, tt.window); got != tt.want {
			t.Errorf("percent(%d,%d) = %d, want %d", tt.tokens, tt.window, got, tt.want)
		}
	}
}
