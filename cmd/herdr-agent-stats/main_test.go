package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/daemon"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/herdr"
)

func TestStateDir(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	got, _ := stateDir(func(string) string { return "/custom" }, home)
	if got != "/custom" {
		t.Errorf("env set: got %q", got)
	}
	got, _ = stateDir(func(string) string { return "" }, home)
	if want := "/home/u/.local/state/herdr/plugins/jmarcosg.agent-stats"; got != want {
		t.Errorf("unset: got %q, want %q", got, want)
	}
}

type fakeTicker struct{ ticks, clears atomic.Int32 }

func (f *fakeTicker) Tick(context.Context) error  { f.ticks.Add(1); return nil }
func (f *fakeTicker) Clear(context.Context) error { f.clears.Add(1); return nil }

func TestSecondDaemonExits(t *testing.T) {
	dir := t.TempDir()
	held, err := tryLock(dir)
	if err != nil || held == nil {
		t.Fatalf("test lock: %v %v", held, err)
	}
	defer held.Close()
	defer func(a int, r time.Duration) { lockAttempts, lockRetry = a, r }(lockAttempts, lockRetry)
	lockAttempts, lockRetry = 3, 10*time.Millisecond
	f := &fakeTicker{}
	if err := runDaemon(context.Background(), dir, f, time.Hour); err != nil {
		t.Fatal(err)
	}
	if f.ticks.Load() != 0 {
		t.Error("second daemon must not tick")
	}
}

func TestDaemonRetriesLockHeldByProbe(t *testing.T) {
	defer func(a int, r time.Duration) { lockAttempts, lockRetry = a, r }(lockAttempts, lockRetry)
	lockAttempts, lockRetry = 20, 10*time.Millisecond
	dir := t.TempDir()
	probe, err := tryLock(dir)
	if err != nil || probe == nil {
		t.Fatalf("probe lock: %v %v", probe, err)
	}
	f := &fakeTicker{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, dir, f, time.Hour) }()
	time.Sleep(100 * time.Millisecond)
	probe.Close()

	waitFor(t, func() bool { _, ok := readPID(dir); return ok && f.ticks.Load() >= 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonLifecycle(t *testing.T) {
	dir := t.TempDir()
	f := &fakeTicker{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, dir, f, time.Hour) }()

	waitFor(t, func() bool { pid, ok := readPID(dir); return ok && pid == os.Getpid() })
	if err := signalDaemon(dir, syscall.SIGUSR1); err != nil { // nudge
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.ticks.Load() >= 2 })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.clears.Load() != 1 {
		t.Errorf("clears = %d, want 1", f.clears.Load())
	}
	if _, ok := readPID(dir); ok {
		t.Error("pid file should be removed")
	}
	if l, _ := tryLock(dir); l == nil {
		t.Error("lock should be free after exit")
	} else {
		l.Close()
	}
}

func TestNudgeAndStopWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	if err := signalDaemon(dir, syscall.SIGUSR1); err != nil {
		t.Error(err)
	}
	if err := stopDaemon(dir, time.Second); err != nil {
		t.Error(err)
	}
	// stale pid of a dead process
	os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(strconv.Itoa(1<<22+12345)), 0o644)
	if err := stopDaemon(dir, time.Second); err != nil {
		t.Error(err)
	}
}

func TestStalePidFileIsNeverSignalled(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pidFile := filepath.Join(dir, "daemon.pid")

	for name, f := range map[string]func() error{
		"nudge": func() error { return signalDaemon(dir, syscall.SIGUSR1) },
		"stop":  func() error { return stopDaemon(dir, time.Second) },
	} {
		os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if syscall.Kill(cmd.Process.Pid, 0) != nil {
			t.Fatalf("%s signalled the unrelated process", name)
		}
		if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
			t.Errorf("%s: stale pid file not removed", name)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

type fakeHerdr struct{ reported bool }

func (fakeHerdr) Agents(context.Context) ([]herdr.Agent, error) {
	return []herdr.Agent{{PaneID: "p1", Kind: "claude"}}, nil
}
func (fakeHerdr) ProcessInfo(context.Context, string) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{ShellPID: 10, ForegroundPGID: 11}, nil
}
func (f *fakeHerdr) ReportTokens(context.Context, string, map[string]string, []string) error {
	f.reported = true
	return nil
}

type fakeDet struct{}

func (fakeDet) Read(agentctx.Pane) (agentctx.Reading, bool) {
	return agentctx.Reading{Model: "Opus 5.5", Percent: 17, HasPercent: true}, true
}

func TestOncePrintsReadingsWithoutReporting(t *testing.T) {
	h := &fakeHerdr{}
	var out bytes.Buffer
	err := runOnce(context.Background(), &out, daemon.Deps{
		Herdr:   h,
		Detect:  func(string) agentctx.Detector { return fakeDet{} },
		Measure: func(int) (uint64, bool) { return 1288490189, true },
		Start:   func(int) (time.Time, bool) { return time.Time{}, false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.reported {
		t.Error("once must not report to herdr")
	}
	var rows []row
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Pane != "p1" || rows[0].Model != "Opus 5.5" ||
		rows[0].Percent == nil || *rows[0].Percent != 17 || rows[0].Mem != "1.2 GB" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestStopWaitsPastClearTimeout(t *testing.T) {
	if stopWait <= clearTimeout {
		t.Errorf("stopWait %v must exceed clearTimeout %v or stop reports a slow clear as a failure", stopWait, clearTimeout)
	}
}

func TestManifestRestartDetachesDaemon(t *testing.T) {
	b, err := os.ReadFile("../../herdr-plugin.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := `command = ["sh", "-c", "./herdr-agent-stats stop; nohup ./herdr-agent-stats daemon </dev/null >/dev/null 2>&1 &"]`
	if !bytes.Contains(b, []byte(want)) {
		t.Errorf("restart action must detach the daemon; manifest lacks %s", want)
	}
}

func TestTickErrorsLoggedOnceThenRecovered(t *testing.T) {
	var lines []string
	e := &errorLog{logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	boom := errors.New("boom")
	for i := 0; i < 3; i++ {
		e.observe(boom)
	}
	if len(lines) != 1 {
		t.Fatalf("repeated error logged %d times: %q", len(lines), lines)
	}
	e.observe(errors.New("other"))
	if len(lines) != 2 {
		t.Fatalf("a different error must be logged: %q", lines)
	}
	e.observe(nil)
	e.observe(nil)
	if len(lines) != 3 || !strings.Contains(lines[2], "recovered") {
		t.Fatalf("want a single recovered line: %q", lines)
	}
	e.observe(boom)
	if len(lines) != 4 {
		t.Errorf("a failure after recovery must be logged again: %q", lines)
	}
}
