//go:build darwin

package process

/*
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>

static int children(int pid, int *buf, int bytes) {
	return proc_listchildpids(pid, buf, bytes);
}

static int all(int *buf, int bytes) {
	return proc_listallpids(buf, bytes);
}

static int footprint(int pid, unsigned long long *out) {
	struct rusage_info_v0 info;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V0, (rusage_info_t *)&info) != 0) return -1;
	*out = info.ri_phys_footprint;
	return 0;
}

static int start_time(int pid, unsigned long long *sec, unsigned long long *usec) {
	struct proc_bsdinfo info;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof(info)) != (int)sizeof(info)) return -1;
	*sec = info.pbi_start_tvsec;
	*usec = info.pbi_start_tvusec;
	return 0;
}

static int cwd(int pid, char *out, int cap) {
	struct proc_vnodepathinfo info;
	if (proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &info, sizeof(info)) != (int)sizeof(info)) return -1;
	strncpy(out, info.pvi_cdir.vip_path, cap - 1);
	out[cap - 1] = 0;
	return 0;
}
*/
import "C"

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Children lists the direct children of pid.
func Children(pid int) []int {
	buf := make([]C.int, 1024)
	n := int(C.children(C.int(pid), &buf[0], C.int(len(buf)*int(unsafe.Sizeof(buf[0])))))
	if n <= 0 {
		return nil
	}
	if n > len(buf) {
		n = len(buf)
	}
	var pids []int
	for _, p := range buf[:n] {
		if p > 0 {
			pids = append(pids, int(p))
		}
	}
	return pids
}

// All lists every process visible to the caller.
func All() []int {
	buf := make([]C.int, 8192)
	n := int(C.all(&buf[0], C.int(len(buf)*int(unsafe.Sizeof(buf[0])))))
	if n <= 0 {
		return nil
	}
	// proc_listallpids returns the number of pids, not bytes.
	if n > len(buf) {
		n = len(buf)
	}
	var pids []int
	for _, p := range buf[:n] {
		if p > 0 {
			pids = append(pids, int(p))
		}
	}
	return pids
}

// Footprint is the process physical footprint in bytes.
func Footprint(pid int) (uint64, bool) {
	var out C.ulonglong
	if C.footprint(C.int(pid), &out) != 0 {
		return 0, false
	}
	return uint64(out), true
}

// StartTime reports when the process started.
func StartTime(pid int) (time.Time, bool) {
	var sec, usec C.ulonglong
	if C.start_time(C.int(pid), &sec, &usec) != 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(sec), int64(usec)*1000), true
}

func procargs(pid int) []byte {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(buf) == 0 {
		return nil
	}
	return buf
}

// Env returns the environment the process started with, or nil if unreadable.
func Env(pid int) map[string]string {
	buf := procargs(pid)
	if buf == nil {
		return nil
	}
	_, env := parseProcargs(buf)
	return env
}

// Argv returns the process arguments, or nil if unreadable.
func Argv(pid int) []string {
	buf := procargs(pid)
	if buf == nil {
		return nil
	}
	argv, _ := parseProcargs(buf)
	return argv
}

// Cwd returns the working directory, or "" if unreadable.
func Cwd(pid int) string {
	out := make([]C.char, C.PROC_PIDPATHINFO_MAXSIZE)
	if C.cwd(C.int(pid), &out[0], C.int(len(out))) != 0 {
		return ""
	}
	return C.GoString(&out[0])
}

// OpenFiles returns absolute paths of the open regular files, via lsof.
func OpenFiles(pid int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-a", "-p", strconv.Itoa(pid), "-Ftn").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var files []string
	regular := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "f"): // new fd record
			regular = false
		case strings.HasPrefix(line, "t"):
			regular = line == "tREG"
		case strings.HasPrefix(line, "n"):
			if path := line[1:]; regular && filepath.IsAbs(path) {
				files = append(files, path)
			}
		}
	}
	return files
}
