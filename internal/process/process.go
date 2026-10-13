// Package process inspects other processes (children, memory, environment)
// through OS-specific primitives.
package process

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// Descendants returns the descendants of pid in breadth-first order, never
// including pid itself, stopping once max have been found.
func Descendants(pid, max int) []int {
	found := Children(pid)
	for at := 0; at < len(found) && len(found) < max; at++ {
		found = append(found, Children(found[at])...)
	}
	if len(found) > max {
		found = found[:max]
	}
	return found
}

// parseProcargs decodes a darwin kern.procargs2 buffer: int32 argc, the exec
// path, NUL padding, argc arguments, then KEY=VAL environment strings up to
// the first empty entry. Both results are nil for a buffer too short to hold
// argc.
func parseProcargs(buf []byte) (argv []string, env map[string]string) {
	if len(buf) < 4 {
		return nil, nil
	}
	argc := int(int32(binary.NativeEndian.Uint32(buf)))
	rest := buf[4:]
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = bytes.TrimLeft(rest[i:], "\x00")
	} else {
		rest = nil
	}

	env = map[string]string{}
	for i, entry := range splitNUL(rest) {
		if i < argc {
			if entry != "" {
				argv = append(argv, entry)
			}
			continue
		}
		if entry == "" {
			break
		}
		if k, v, ok := strings.Cut(entry, "="); ok && k != "" {
			env[k] = v
		}
	}
	return argv, env
}

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// parseNULVars decodes /proc/<pid>/environ style data.
func parseNULVars(data []byte) map[string]string {
	env := map[string]string{}
	for _, entry := range splitNUL(data) {
		if k, v, ok := strings.Cut(entry, "="); ok && k != "" {
			env[k] = v
		}
	}
	return env
}

// statusFootprint derives (RssAnon + VmSwap) in bytes from /proc/<pid>/status
// text. RssAnon is required; VmSwap defaults to zero.
func statusFootprint(status string) (uint64, bool) {
	kib := func(key string) (uint64, bool) {
		for _, line := range strings.Split(status, "\n") {
			rest, ok := strings.CutPrefix(line, key)
			if !ok {
				continue
			}
			num, ok := strings.CutSuffix(strings.TrimSpace(rest), "kB")
			if !ok {
				return 0, false
			}
			n, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64)
			return n, err == nil
		}
		return 0, false
	}
	anon, ok := kib("RssAnon:")
	if !ok {
		return 0, false
	}
	swap, _ := kib("VmSwap:")
	total := anon + swap
	if total < anon {
		total = ^uint64(0)
	}
	if total > ^uint64(0)/1024 {
		return ^uint64(0), true
	}
	return total * 1024, true
}
