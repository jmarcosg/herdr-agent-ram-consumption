package claude

import (
	"strconv"
	"strings"
)

const (
	standardWindow = 200_000
	longWindow     = 1_000_000
	longSuffix     = "[1m]"

	envNoLong    = "CLAUDE_CODE_DISABLE_1M_CONTEXT"
	envMaxTokens = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	envNoCompact = "DISABLE_COMPACT"
)

// Window returns the context window for a reply from model that used tokens.
// requested is the model the session asked for (--model, ANTHROPIC_MODEL or
// settings), which may carry a "[1m]" suffix.
func Window(model string, tokens int, env map[string]string, requested string) int {
	v := func(name string) string { return strings.TrimSpace(env[name]) }
	maxTokens := 0
	if n, err := strconv.Atoi(v(envMaxTokens)); err == nil && n > 0 {
		maxTokens = n
	}
	if maxTokens > 0 && truthy(v(envNoCompact)) {
		return maxTokens
	}
	fam, ver, known := parseModel(model)
	long := known && nativeLong(fam, ver) || asksLong(requested, model)
	if long && !truthy(v(envNoLong)) {
		return longWindow
	}
	if !known && maxTokens > 0 {
		return maxTokens
	}
	if tokens > standardWindow {
		return longWindow
	}
	return standardWindow
}

// percent is round(tokens*100/window), capped at 100.
func percent(tokens, window int) int {
	window = max(window, 1)
	return min((tokens*100+window/2)/window, 100)
}

func truthy(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func asksLong(requested, model string) bool {
	r := strings.ToLower(strings.TrimSpace(requested))
	base, ok := strings.CutSuffix(r, longSuffix)
	return ok && base != "" && strings.Contains(strings.ToLower(model), base)
}

func nativeLong(family string, version []int) bool {
	major, minor := 0, 0
	if len(version) > 0 {
		major = version[0]
	}
	if len(version) > 1 {
		minor = version[1]
	}
	return major >= 5 || family == "opus" && (major > 4 || major == 4 && minor >= 7)
}

// parseModel extracts family and version numbers (1-2 digit tokens) from ids
// like "claude-opus-4-7", "us.anthropic.claude-opus-4-1-20250805-v1:0".
func parseModel(id string) (family string, version []int, ok bool) {
	s := strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if _, rest, found := strings.Cut(s, "anthropic."); found {
		s = rest
	}
	if i := strings.IndexAny(s, "@["); i >= 0 {
		s = s[:i]
	}
	rest, found := strings.CutPrefix(s, "claude-")
	if !found {
		return "", nil, false
	}
	for _, t := range strings.Split(rest, "-") {
		switch {
		case family == "" && isAlpha(t):
			family = t
		case len(t) >= 1 && len(t) <= 2 && isDigits(t):
			n, _ := strconv.Atoi(t)
			version = append(version, n)
		}
	}
	return family, version, family != ""
}

func isAlpha(t string) bool {
	for i := 0; i < len(t); i++ {
		if t[i] < 'a' || t[i] > 'z' {
			return false
		}
	}
	return t != ""
}

func isDigits(t string) bool {
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return false
		}
	}
	return t != ""
}
