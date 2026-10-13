// Package model turns raw model ids into short display names.
package model

import (
	"strconv"
	"strings"
)

// Pretty maps ids like "claude-sonnet-4-5-20250929" to "Sonnet 4.5".
// Ids it cannot parse are returned unchanged.
func Pretty(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if _, rest, ok := strings.Cut(s, "anthropic."); ok {
		s = rest
	}
	if i := strings.IndexAny(s, "@["); i >= 0 {
		s = s[:i]
	}
	rest, ok := strings.CutPrefix(s, "claude-")
	if !ok {
		return id
	}

	family := ""
	var version []int
	for _, t := range strings.Split(rest, "-") {
		switch {
		case family == "" && isAlpha(t):
			family = t
		case len(t) >= 1 && len(t) <= 2 && isDigits(t):
			n, _ := strconv.Atoi(t)
			version = append(version, n)
		}
	}
	if family == "" {
		return id
	}

	name := strings.ToUpper(family[:1]) + family[1:]
	if len(version) == 2 && version[1] == 0 {
		version = version[:1]
	}
	if len(version) == 0 {
		return name
	}
	parts := make([]string, len(version))
	for i, v := range version {
		parts[i] = strconv.Itoa(v)
	}
	return name + " " + strings.Join(parts, ".")
}

func isAlpha(t string) bool  { return t != "" && allBytes(t, 'a', 'z') }
func isDigits(t string) bool { return t != "" && allBytes(t, '0', '9') }

func allBytes(t string, lo, hi byte) bool {
	for i := 0; i < len(t); i++ {
		if t[i] < lo || t[i] > hi {
			return false
		}
	}
	return true
}
