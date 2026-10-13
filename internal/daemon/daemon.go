// Package daemon polls herdr for agent panes and publishes model, context and
// memory tokens, sending only what changed.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/format"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/herdr"
)

// callTimeout bounds every herdr call (the CLI sets none of its own).
const callTimeout = 5 * time.Second

// Herdr is the slice of the herdr client the daemon uses.
type Herdr interface {
	Agents(ctx context.Context) ([]herdr.Agent, error)
	ProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	ReportTokens(ctx context.Context, paneID string, set map[string]string, clear []string) error
}

// Deps are the daemon's collaborators.
type Deps struct {
	Herdr   Herdr
	Detect  func(kind string) agentctx.Detector // nil result: kind has no detector
	Measure func(root int) (uint64, bool)       // RAM of a process tree
	Start   func(pid int) (time.Time, bool)
	Log     func(format string, args ...any)
}

type paneState struct {
	kind     string
	pid      int
	detector agentctx.Detector
	last     map[string]string // tokens as last reported
}

// Daemon holds per-pane state. It is not safe for concurrent use.
type Daemon struct {
	d     Deps
	panes map[string]*paneState
}

// New returns a Daemon using d.
func New(d Deps) *Daemon {
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return &Daemon{d: d, panes: map[string]*paneState{}}
}

// Tick polls once and reports token changes. It fails only when the agent list
// cannot be read; per-pane problems are logged and skipped.
func (d *Daemon) Tick(ctx context.Context) error {
	agents, err := withTimeout(ctx, d.d.Herdr.Agents)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		seen[a.PaneID] = true
		if err := d.tickPane(ctx, a); err != nil {
			d.d.Log("pane %s: %v", a.PaneID, err)
		}
	}
	// The agent left but its pane may survive: clear what we wrote. Keep the
	// state on failure so the next tick (or Clear) retries.
	for id, st := range d.panes {
		if seen[id] {
			continue
		}
		if len(st.last) == 0 {
			delete(d.panes, id)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := d.d.Herdr.ReportTokens(cctx, id, nil, slices.Sorted(maps.Keys(st.last)))
		cancel()
		if err == nil || errors.Is(err, herdr.ErrPaneGone) {
			delete(d.panes, id)
			continue
		}
		d.d.Log("pane %s: clear departed: %v", id, err)
	}
	return nil
}

func (d *Daemon) tickPane(ctx context.Context, a herdr.Agent) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	info, err := d.d.Herdr.ProcessInfo(cctx, a.PaneID)
	cancel()
	if errors.Is(err, herdr.ErrPaneGone) {
		delete(d.panes, a.PaneID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("process info: %w", err)
	}

	proc, pid := findAgent(a.Kind, info)
	st, tracked := d.panes[a.PaneID]
	if !tracked {
		st = &paneState{}
		d.panes[a.PaneID] = st
	}
	// A new pid or kind means a different agent: fresh detector, but keep last
	// so tokens the new agent cannot supply get cleared.
	if !tracked || st.pid != pid || st.kind != a.Kind {
		st.kind, st.pid = a.Kind, pid
		st.detector = d.d.Detect(a.Kind)
	}

	var r agentctx.Reading
	known := false
	if st.detector != nil {
		pane := agentctx.Pane{
			ID: a.PaneID, Kind: a.Kind, SessionID: a.SessionID,
			Cwd: a.Cwd, AgentPID: pid, Argv: proc.Argv,
		}
		if len(pane.Argv) == 0 && proc.Argv0 != "" {
			pane.Argv = []string{proc.Argv0}
		}
		if proc.Cwd != "" {
			pane.Cwd = proc.Cwd
		}
		if t, ok := d.d.Start(pid); ok {
			pane.Start = t
		}
		r, known = st.detector.Read(pane)
	}
	mem, memOK := d.d.Measure(info.ShellPID)
	return d.report(ctx, a.PaneID, st, Tokens(r, known, mem, memOK))
}

func (d *Daemon) report(ctx context.Context, paneID string, st *paneState, want map[string]string) error {
	set := map[string]string{}
	for k, v := range want {
		if old, ok := st.last[k]; !ok || old != v {
			set[k] = v
		}
	}
	var clear []string
	for k := range st.last {
		if _, ok := want[k]; !ok {
			clear = append(clear, k)
		}
	}
	if len(set) == 0 && len(clear) == 0 {
		return nil
	}
	slices.Sort(clear)
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := d.d.Herdr.ReportTokens(cctx, paneID, set, clear); err != nil {
		return fmt.Errorf("report tokens: %w", err)
	}
	st.last = maps.Clone(want)
	return nil
}

// Clear removes every token the daemon has set.
func (d *Daemon) Clear(ctx context.Context) error {
	var errs []error
	for id, st := range d.panes {
		if len(st.last) == 0 {
			continue
		}
		keys := slices.Sorted(maps.Keys(st.last))
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := d.d.Herdr.ReportTokens(cctx, id, nil, keys)
		cancel()
		if err != nil && !errors.Is(err, herdr.ErrPaneGone) {
			errs = append(errs, fmt.Errorf("clear %s: %w", id, err))
			continue
		}
		st.last = nil
	}
	return errors.Join(errs...)
}

func withTimeout[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return f(ctx)
}

// Tokens builds the sidebar tokens for a pane.
func Tokens(r agentctx.Reading, known bool, mem uint64, memOK bool) map[string]string {
	t := map[string]string{}
	if known && r.Model != "" {
		t["agent_model"] = r.Model
	}
	if known && r.HasPercent {
		t["agent_ctx_"+format.Severity(r.Percent)] = fmt.Sprintf("%d%%", r.Percent)
	}
	if memOK {
		t["agent_mem"] = format.Size(mem)
	}
	return t
}

// AgentPID returns the pid of the agent process in info, or the foreground
// process group when none matches.
func AgentPID(kind string, info herdr.ProcessInfo) int {
	_, pid := findAgent(kind, info)
	return pid
}

func findAgent(kind string, info herdr.ProcessInfo) (herdr.Proc, int) {
	for _, p := range info.Procs {
		if isAgent(kind, p) {
			return p, p.PID
		}
	}
	for _, p := range info.Procs {
		if p.PID == info.ForegroundPGID {
			return p, p.PID
		}
	}
	return herdr.Proc{}, info.ForegroundPGID
}

func isAgent(kind string, p herdr.Proc) bool {
	if filepath.Base(p.Argv0) == kind {
		return true
	}
	if len(p.Argv) == 0 {
		return false
	}
	if filepath.Base(p.Argv[0]) == kind {
		return true
	}
	// gemini and codex run as `node /path/to/<kind> ...`.
	if filepath.Base(p.Argv[0]) == "node" {
		for _, a := range p.Argv[1:] {
			if len(a) > 0 && a[0] != '-' {
				return filepath.Base(a) == kind
			}
		}
	}
	return false
}
