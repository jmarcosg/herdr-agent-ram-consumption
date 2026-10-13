package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/herdr"
)

type report struct {
	pane  string
	set   map[string]string
	clear []string
}

type fakeHerdr struct {
	agents    []herdr.Agent
	agentsErr error
	infos     map[string]herdr.ProcessInfo
	infoErr   map[string]error
	reports   []report
	reportErr error
	clearErr  map[string]error // per-pane error for clear-only calls
}

func (f *fakeHerdr) Agents(context.Context) ([]herdr.Agent, error) { return f.agents, f.agentsErr }
func (f *fakeHerdr) ProcessInfo(_ context.Context, id string) (herdr.ProcessInfo, error) {
	return f.infos[id], f.infoErr[id]
}
func (f *fakeHerdr) ReportTokens(_ context.Context, id string, set map[string]string, clear []string) error {
	f.reports = append(f.reports, report{id, set, clear})
	if len(set) == 0 && f.clearErr[id] != nil {
		return f.clearErr[id]
	}
	return f.reportErr
}

type fakeDet struct {
	r     agentctx.Reading
	known bool
	panes []agentctx.Pane
}

func (d *fakeDet) Read(p agentctx.Pane) (agentctx.Reading, bool) {
	d.panes = append(d.panes, p)
	return d.r, d.known
}

func claudeInfo(pid int) herdr.ProcessInfo {
	return herdr.ProcessInfo{ShellPID: 1, ForegroundPGID: pid, Procs: []herdr.Proc{
		{PID: pid, Argv0: "claude", Argv: []string{"claude"}, Cwd: "/agent"},
	}}
}

func newDaemon(h *fakeHerdr, det func(string) agentctx.Detector) *Daemon {
	return New(Deps{
		Herdr:   h,
		Detect:  det,
		Measure: func(int) (uint64, bool) { return 1 << 30, true },
		Start:   func(int) (time.Time, bool) { return time.Time{}, false },
	})
}

func TestTokensFull(t *testing.T) {
	mem := uint64(12 << 30 / 10)
	cases := map[int]string{17: "agent_ctx_ok", 80: "agent_ctx_warn", 95: "agent_ctx_crit"}
	for pct, key := range cases {
		got := Tokens(agentctx.Reading{Model: "Opus 5.5", Percent: pct, HasPercent: true}, true, mem, true)
		want := map[string]string{"agent_model": "Opus 5.5", key: strconv.Itoa(pct) + "%", "agent_mem": "1.2 GB"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pct %d: got %v want %v", pct, got, want)
		}
	}
}

func TestTokensRAMOnly(t *testing.T) {
	got := Tokens(agentctx.Reading{Model: "x", Percent: 5, HasPercent: true}, false, 100<<20, true)
	if want := map[string]string{"agent_mem": "100 MB"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestAgentPID(t *testing.T) {
	b, err := os.ReadFile("../herdr/testdata/process-info.json")
	if err != nil {
		t.Fatal(err)
	}
	cli := herdr.New("herdr", func(context.Context, string, ...string) ([]byte, error) { return b, nil })
	info, err := cli.ProcessInfo(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := AgentPID("claude", info); got != 35452 {
		t.Errorf("claude pid = %d", got)
	}
	if got := AgentPID("codex", info); got != info.ForegroundPGID {
		t.Errorf("fallback = %d want %d", got, info.ForegroundPGID)
	}
}

func TestAgentPIDNode(t *testing.T) {
	info := herdr.ProcessInfo{ForegroundPGID: 1, Procs: []herdr.Proc{
		{PID: 7, Argv0: "node", Argv: []string{"node", "--no-warnings", "/x/bin/gemini", "-m", "y"}},
	}}
	if got := AgentPID("gemini", info); got != 7 {
		t.Errorf("got %d", got)
	}
}

func oneClaude(pct int) (*fakeHerdr, *fakeDet) {
	h := &fakeHerdr{
		agents: []herdr.Agent{{PaneID: "p1", Kind: "claude", Cwd: "/herdr"}},
		infos:  map[string]herdr.ProcessInfo{"p1": claudeInfo(10)},
	}
	return h, &fakeDet{r: agentctx.Reading{Model: "Opus", Percent: pct, HasPercent: true}, known: true}
}

func TestTickSendsDiffOnly(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.reports) != 1 {
		t.Fatalf("reports after identical ticks = %d", len(h.reports))
	}
	det.r.Percent = 80
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 {
		t.Fatalf("reports = %d", len(h.reports))
	}
	got := h.reports[1]
	if !reflect.DeepEqual(got.set, map[string]string{"agent_ctx_warn": "80%"}) ||
		!reflect.DeepEqual(got.clear, []string{"agent_ctx_ok"}) {
		t.Errorf("diff = %+v", got)
	}
}

func TestPaneFields(t *testing.T) {
	h, det := oneClaude(1)
	start := time.Unix(100, 0)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	d.d.Start = func(int) (time.Time, bool) { return start, true }
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := det.panes[0]
	if p.Cwd != "/agent" || p.AgentPID != 10 || !p.Start.Equal(start) || !reflect.DeepEqual(p.Argv, []string{"claude"}) {
		t.Errorf("pane = %+v", p)
	}
}

func TestResetOnAgentChange(t *testing.T) {
	h, _ := oneClaude(17)
	known := &fakeDet{r: agentctx.Reading{Model: "Opus", Percent: 17, HasPercent: true}, known: true}
	unknown := &fakeDet{}
	var made []*fakeDet
	dets := []*fakeDet{known, unknown}
	d := newDaemon(h, func(string) agentctx.Detector {
		x := dets[len(made)]
		made = append(made, x)
		return x
	})
	ctx := context.Background()
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.infos["p1"] = claudeInfo(11)
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 {
		t.Fatalf("detectors requested = %d", len(made))
	}
	got := h.reports[1]
	if !reflect.DeepEqual(got.clear, []string{"agent_ctx_ok", "agent_model"}) || len(got.set) != 0 {
		t.Errorf("reset diff = %+v", got)
	}
}

func TestForgetsClosedPanes(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.agents = nil
	for i := 0; i < 2; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.reports) != 2 || len(d.panes) != 0 {
		t.Fatalf("reports=%d panes=%d", len(h.reports), len(d.panes))
	}
	got := h.reports[1]
	if got.pane != "p1" || len(got.set) != 0 ||
		!reflect.DeepEqual(got.clear, []string{"agent_ctx_ok", "agent_mem", "agent_model"}) {
		t.Errorf("departure clear = %+v", got)
	}
}

func TestDepartedClearRetried(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.agents = nil
	h.clearErr = map[string]error{"p1": errors.New("boom")}
	_ = d.Tick(ctx)
	if len(d.panes) != 1 {
		t.Fatal("state dropped despite failed clear")
	}
	h.clearErr = nil
	_ = d.Tick(ctx)
	if len(h.reports) != 3 || len(d.panes) != 0 {
		t.Errorf("reports=%d panes=%d", len(h.reports), len(d.panes))
	}
}

func TestFailedReportResendsSameSet(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	h.reportErr = errors.New("boom")
	_ = d.Tick(ctx)
	h.reportErr = nil
	_ = d.Tick(ctx)
	if len(h.reports) != 2 || !reflect.DeepEqual(h.reports[0], h.reports[1]) {
		t.Errorf("reports = %+v", h.reports)
	}
}

func TestFailedClearRetried(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.reports = nil
	h.reportErr = errors.New("boom")
	if err := d.Clear(ctx); err == nil {
		t.Fatal("want error")
	}
	h.reportErr = nil
	if err := d.Clear(ctx); err != nil || len(h.reports) != 2 {
		t.Errorf("err=%v reports=%d", err, len(h.reports))
	}
}

func TestPaneGoneDropsState(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.infoErr = map[string]error{"p1": herdr.ErrPaneGone}
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.panes) != 0 || len(h.reports) != 1 {
		t.Errorf("panes=%d reports=%d", len(d.panes), len(h.reports))
	}
}

func TestTickSurvivesHerdrError(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	h.agentsErr = errors.New("boom")
	if err := d.Tick(context.Background()); err == nil {
		t.Fatal("want error")
	}
	h.agentsErr = nil
	if err := d.Tick(context.Background()); err != nil || len(h.reports) != 1 {
		t.Errorf("err=%v reports=%d", err, len(h.reports))
	}
}

func TestClear(t *testing.T) {
	h, det := oneClaude(17)
	h.agents = append(h.agents, herdr.Agent{PaneID: "p2", Kind: "claude"})
	h.infos["p2"] = claudeInfo(20)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.reports = nil
	if err := d.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 {
		t.Fatalf("reports = %d", len(h.reports))
	}
	for _, r := range h.reports {
		if !reflect.DeepEqual(r.clear, []string{"agent_ctx_ok", "agent_mem", "agent_model"}) {
			t.Errorf("%s clear = %v", r.pane, r.clear)
		}
	}
}

func TestFirstSightClearsEveryOtherKnownToken(t *testing.T) {
	h := &fakeHerdr{
		agents: []herdr.Agent{{PaneID: "p1", Kind: "claude"}},
		infos:  map[string]herdr.ProcessInfo{"p1": claudeInfo(10)},
	}
	d := newDaemon(h, func(string) agentctx.Detector { return nil })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.reports) != 1 {
		t.Fatalf("reports = %d, want 1 (second identical tick sends nothing)", len(h.reports))
	}
	got := h.reports[0]
	if !reflect.DeepEqual(got.set, map[string]string{"agent_mem": "1.0 GB"}) ||
		!reflect.DeepEqual(got.clear, []string{"agent_ctx_crit", "agent_ctx_ok", "agent_ctx_warn", "agent_model"}) {
		t.Errorf("first tick = %+v", got)
	}
}

func TestFirstSightClearRetriedAfterFailedReport(t *testing.T) {
	h := &fakeHerdr{
		agents: []herdr.Agent{{PaneID: "p1", Kind: "claude"}},
		infos:  map[string]herdr.ProcessInfo{"p1": claudeInfo(10)},
	}
	d := newDaemon(h, func(string) agentctx.Detector { return nil })
	ctx := context.Background()
	h.reportErr = errors.New("boom")
	_ = d.Tick(ctx)
	h.reportErr = nil
	_ = d.Tick(ctx)
	if len(h.reports) != 2 || len(h.reports[1].clear) != 4 {
		t.Errorf("reports = %+v", h.reports)
	}
}

type logRecorder struct{ lines []string }

func (l *logRecorder) log(f string, a ...any) { l.lines = append(l.lines, fmt.Sprintf(f, a...)) }

func withLog(d *Daemon) *logRecorder {
	l := &logRecorder{}
	d.d.Log = l.log
	return l
}

func TestDepartedClearFailureLoggedOncePerDistinctError(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	l := withLog(d)
	ctx := context.Background()
	_ = d.Tick(ctx)
	h.agents = nil
	h.clearErr = map[string]error{"p1": errors.New("boom")}
	for i := 0; i < 4; i++ {
		_ = d.Tick(ctx)
	}
	if len(l.lines) != 1 {
		t.Fatalf("same error logged %d times: %q", len(l.lines), l.lines)
	}
	h.clearErr = map[string]error{"p1": errors.New("other")}
	_ = d.Tick(ctx)
	if len(l.lines) != 2 {
		t.Errorf("a different error must be logged: %q", l.lines)
	}
}

func TestPaneErrorLoggedOnceUntilItChanges(t *testing.T) {
	h, det := oneClaude(17)
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	l := withLog(d)
	ctx := context.Background()
	h.infoErr = map[string]error{"p1": errors.New("boom")}
	for i := 0; i < 3; i++ {
		_ = d.Tick(ctx)
	}
	if len(l.lines) != 1 {
		t.Fatalf("same pane error logged %d times: %q", len(l.lines), l.lines)
	}
	h.infoErr = nil
	_ = d.Tick(ctx)
	h.infoErr = map[string]error{"p1": errors.New("boom")}
	_ = d.Tick(ctx)
	if len(l.lines) != 2 {
		t.Errorf("an error after recovery must be logged again: %q", l.lines)
	}
}

func TestNoMeasureWhenShellPIDNotPositive(t *testing.T) {
	h, det := oneClaude(17)
	info := claudeInfo(10)
	info.ShellPID = 0
	h.infos["p1"] = info
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	d.d.Measure = func(int) (uint64, bool) { t.Error("Measure called with shell pid 0"); return 1, true }
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.reports[0].set["agent_mem"]; ok || len(h.reports) != 1 {
		t.Errorf("reports = %+v", h.reports)
	}
}

func TestNoDetectionWhenAgentPIDNotPositive(t *testing.T) {
	h, det := oneClaude(17)
	h.infos["p1"] = herdr.ProcessInfo{ShellPID: 1}
	d := newDaemon(h, func(string) agentctx.Detector { return det })
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(det.panes) != 0 {
		t.Errorf("detector read with agent pid 0: %+v", det.panes)
	}
	if want := map[string]string{"agent_mem": "1.0 GB"}; !reflect.DeepEqual(h.reports[0].set, want) {
		t.Errorf("set = %v, want RAM only", h.reports[0].set)
	}
}

type panicDet struct{}

func (panicDet) Read(agentctx.Pane) (agentctx.Reading, bool) { panic("kaboom") }

func TestPanickingDetectorSkipsOnlyThatPane(t *testing.T) {
	h, det := oneClaude(17)
	h.agents = append(h.agents, herdr.Agent{PaneID: "p2", Kind: "codex"})
	h.infos["p2"] = claudeInfo(20)
	d := newDaemon(h, func(kind string) agentctx.Detector {
		if kind == "codex" {
			return panicDet{}
		}
		return det
	})
	l := withLog(d)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].pane != "p1" {
		t.Errorf("reports = %+v, want only p1", h.reports)
	}
	if len(l.lines) != 1 || !strings.Contains(l.lines[0], "kaboom") {
		t.Errorf("log = %q", l.lines)
	}
	if _, ok := d.panes["p2"]; !ok {
		t.Error("panicking pane must keep its state")
	}
}
