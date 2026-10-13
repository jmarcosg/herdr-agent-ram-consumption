// Command herdr-agent-stats shows model, context usage and RAM for every agent
// in herdr's Agents sidebar.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/daemon"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/detect"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/herdr"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/memory"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/process"
)

const (
	tickEvery    = 2 * time.Second
	clearTimeout = 5 * time.Second
	stopWait     = 3 * time.Second
	descendCap   = 256
)

// Package vars so tests can shrink the startup lock retry window.
var (
	lockAttempts = 5
	lockRetry    = 50 * time.Millisecond
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-agent-stats daemon|nudge|stop|once")
		os.Exit(2)
	}
	dir, err := stateDir(os.Getenv, os.UserHomeDir)
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err == nil {
		err = run(os.Args[1], dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-agent-stats:", err)
		os.Exit(1)
	}
}

func run(cmd, dir string) error {
	switch cmd {
	case "daemon":
		f, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		log.SetOutput(f)
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return runDaemon(ctx, dir, daemon.New(newDeps(f)), tickEvery)
	case "nudge":
		return signalDaemon(dir, syscall.SIGUSR1)
	case "stop":
		return stopDaemon(dir, stopWait)
	case "once":
		return runOnce(context.Background(), os.Stdout, newDeps(io.Discard))
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// stateDir is HERDR_PLUGIN_STATE_DIR or the per-user default.
func stateDir(getenv func(string) string, home func() (string, error)) (string, error) {
	if d := getenv("HERDR_PLUGIN_STATE_DIR"); d != "" {
		return d, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".local", "state", "herdr", "plugins", "jmarcosg.agent-stats"), nil
}

func host() agentctx.Host {
	home, _ := os.UserHomeDir()
	proc := func(pid int) agentctx.Proc {
		return agentctx.Proc{PID: pid, Argv: process.Argv(pid), Cwd: process.Cwd(pid)}
	}
	return agentctx.Host{
		Home:      home,
		Getenv:    os.Getenv,
		Env:       process.Env,
		OpenFiles: process.OpenFiles,
		Descendants: func(pid int) []agentctx.Proc {
			var out []agentctx.Proc
			for _, p := range process.Descendants(pid, descendCap) {
				out = append(out, proc(p))
			}
			return out
		},
		Processes: func() []agentctx.Proc {
			var out []agentctx.Proc
			for _, p := range process.All() {
				out = append(out, proc(p))
			}
			return out
		},
	}
}

func newDeps(w io.Writer) daemon.Deps {
	h := host()
	logger := log.New(w, "", log.LstdFlags)
	return daemon.Deps{
		Herdr: herdr.New(herdr.BinPath(os.Getenv), herdr.ExecRunner),
		Detect: func(kind string) agentctx.Detector {
			return detect.New(kind, h)
		},
		Measure: memory.Measure,
		Start:   process.StartTime,
		Log:     logger.Printf,
	}
}

// ticker is the part of the daemon the loop drives.
type ticker interface {
	Tick(ctx context.Context) error
	Clear(ctx context.Context) error
}

// runDaemon holds the instance lock, ticks until ctx ends or SIGUSR1 asks for
// an immediate tick, then clears the tokens it wrote. A second instance
// returns nil at once.
func runDaemon(ctx context.Context, dir string, d ticker, every time.Duration) error {
	// A nudge/stop probe may hold the lock for a moment; retry before concluding
	// another daemon runs.
	var lock *os.File
	for i := 0; i < lockAttempts; i++ {
		var err error
		if lock, err = tryLock(dir); err != nil {
			return err
		}
		if lock != nil {
			break
		}
		if i < lockAttempts-1 {
			time.Sleep(lockRetry)
		}
	}
	if lock == nil {
		return nil
	}
	defer lock.Close() // closing the fd releases the flock

	// Handle SIGUSR1 before the pid file exists: an early nudge must not kill us.
	// (SIGTERM/SIGINT are handled by the caller's NotifyContext.)
	nudge := make(chan os.Signal, 1)
	signal.Notify(nudge, syscall.SIGUSR1)
	defer signal.Stop(nudge)

	pidFile := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidFile)

	tick := func() {
		if err := d.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Printf("tick: %v", err)
		}
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.Background(), clearTimeout)
			defer cancel()
			if err := d.Clear(cctx); err != nil {
				log.Printf("clear: %v", err)
			}
			return nil
		case <-t.C:
			tick()
		case <-nudge:
			tick()
		}
	}
}

// tryLock takes the exclusive daemon lock; (nil, nil) means it is held.
func tryLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

func readPID(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

// livePID returns the daemon's pid only if the lock is held by someone else.
// A free lock means no daemon runs, so a leftover pid file is stale (the pid may
// have been reused): it is removed and never signalled.
func livePID(dir string) (int, bool) {
	l, err := tryLock(dir)
	if err != nil {
		return 0, false
	}
	if l != nil {
		os.Remove(filepath.Join(dir, "daemon.pid"))
		l.Close()
		return 0, false
	}
	return readPID(dir)
}

// signalDaemon sends sig to the running daemon; no daemon is not an error.
func signalDaemon(dir string, sig syscall.Signal) error {
	pid, ok := livePID(dir)
	if !ok {
		return nil
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// stopDaemon asks the daemon to exit and waits for its lock to free.
func stopDaemon(dir string, wait time.Duration) error {
	if _, ok := livePID(dir); !ok {
		return nil
	}
	if err := signalDaemon(dir, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		l, err := tryLock(dir)
		if err != nil {
			return err
		}
		if l != nil {
			return l.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("daemon did not stop in time")
}

// capture records what the daemon would report instead of sending it.
type capture struct {
	daemon.Herdr
	tokens map[string]map[string]string
}

func (c *capture) ReportTokens(_ context.Context, pane string, set map[string]string, _ []string) error {
	c.tokens[pane] = set
	return nil
}

type row struct {
	Pane    string `json:"pane"`
	Model   string `json:"model"`
	Percent *int   `json:"percent"`
	Mem     string `json:"mem"`
}

// runOnce polls one time without reporting and prints the readings as JSON.
func runOnce(ctx context.Context, w io.Writer, deps daemon.Deps) error {
	c := &capture{Herdr: deps.Herdr, tokens: map[string]map[string]string{}}
	deps.Herdr = c
	if err := daemon.New(deps).Tick(ctx); err != nil {
		return err
	}
	rows := []row{}
	for pane, t := range c.tokens {
		r := row{Pane: pane, Model: t["agent_model"], Mem: t["agent_mem"]}
		for _, sev := range []string{"ok", "warn", "crit"} {
			if v, ok := t["agent_ctx_"+sev]; ok {
				if n, err := strconv.Atoi(strings.TrimSuffix(v, "%")); err == nil {
					r.Percent = &n
				}
			}
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.Pane, b.Pane) })
	return json.NewEncoder(w).Encode(rows)
}
