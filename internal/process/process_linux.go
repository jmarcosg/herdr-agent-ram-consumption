//go:build linux

package process

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const clockTicks = 100 // CLK_TCK is 100 on every Linux ABI we target

// Children lists the direct children of pid, merged across all its threads.
func Children(pid int) []int {
	tasks, _ := filepath.Glob("/proc/" + strconv.Itoa(pid) + "/task/*/children")
	var pids []int
	for _, path := range tasks {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				pids = append(pids, n)
			}
		}
	}
	slices.Sort(pids)
	return slices.Compact(pids)
}

// Footprint is anonymous resident memory plus swap, in bytes.
func Footprint(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	return statusFootprint(string(data))
}

// StartTime is boot time plus the process start offset (stat field 22).
func StartTime(pid int) (time.Time, bool) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false
	}
	// The comm field may contain spaces and parens; fields resume after the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	// fields[0] is field 3 (state), so field 22 is fields[19].
	if len(fields) < 20 {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}
	return boot.Add(time.Duration(ticks) * time.Second / clockTicks), true
}

func bootTime() (time.Time, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			return time.Unix(secs, 0), err == nil
		}
	}
	return time.Time{}, false
}

// Env returns the environment the process started with, or nil if unreadable.
func Env(pid int) map[string]string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil
	}
	return parseNULVars(data)
}

// Argv returns the process arguments, or nil if unreadable.
func Argv(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	var argv []string
	for _, a := range bytes.Split(data, []byte{0}) {
		if len(a) > 0 {
			argv = append(argv, string(a))
		}
	}
	return argv
}

// Cwd returns the working directory, or "" if unreadable.
func Cwd(pid int) string {
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return path
}

// OpenFiles returns absolute paths of the open regular files.
func OpenFiles(pid int) []string {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || !filepath.IsAbs(target) {
			continue
		}
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
			files = append(files, target)
		}
	}
	return files
}
