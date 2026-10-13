package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

const id = "019a1234-5678-7000-8000-000000000001"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type env struct {
	t      *testing.T
	home   string // $HOME
	codex  string // directory holding sessions/
	agent  map[string]string
	open   []string
	calls  int
	rollup string
}

// realTemp is a temp dir with symlinks resolved (macOS /var -> /private/var),
// as lsof reports it.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func setup(t *testing.T, content string) *env {
	t.Helper()
	e := &env{t: t, home: realTemp(t)}
	e.codex = filepath.Join(e.home, ".codex")
	e.rollup = e.addRollout(id, content)
	return e
}

func (e *env) addRollout(uuid, content string) string {
	e.t.Helper()
	dir := filepath.Join(e.codex, "sessions", "2026", "10", "05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-10-05T12-00-00-"+uuid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.open = append(e.open, p)
	return p
}

func (e *env) append(content string) {
	e.t.Helper()
	f, err := os.OpenFile(e.rollup, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) detector() agentctx.Detector {
	return New(agentctx.Host{
		Home: e.home,
		Env:  func(int) map[string]string { return e.agent },
		OpenFiles: func(int) []string {
			e.calls++
			return e.open
		},
		Descendants: func(int) []agentctx.Proc { return nil },
	})
}

var pane = agentctx.Pane{ID: "p1", Kind: "codex", AgentPID: 42}

func read(t *testing.T, d agentctx.Detector) (agentctx.Reading, bool) {
	t.Helper()
	return d.Read(pane)
}

func want(t *testing.T, d agentctx.Detector, model string, percent int, has bool) {
	t.Helper()
	got, ok := read(t, d)
	w := agentctx.Reading{Model: model, Percent: percent, HasPercent: has}
	if !ok || got != w {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, w)
	}
}

func wantNone(t *testing.T, d agentctx.Detector) {
	t.Helper()
	if got, ok := read(t, d); ok {
		t.Errorf("got %+v, want unknown", got)
	}
}

func tokenCount(window, tokens string) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":` + tokens + `},"model_context_window":` + window + `}}}` + "\n"
}

func TestSingleRolloutGivesModelAndPercent(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	want(t, e.detector(), "gpt-5.4", 20, true)
}

func TestModelChangeDropsPercentUntilNextTokenCount(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	e.append(fixture(t, "model-change.jsonl"))
	want(t, d, "gpt-5.4-mini", 0, false)
	e.append(tokenCount("250000", "125000"))
	want(t, d, "gpt-5.4-mini", 50, true)
}

func TestCompactionDropsPercent(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	e.append(fixture(t, "compacted.jsonl"))
	want(t, d, "gpt-5.4", 0, false)
}

func TestCopiedSettingsFromAnotherThreadAreIgnored(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	e.append(strings.ReplaceAll(fixture(t, "model-change.jsonl"), id, "019a1234-5678-7000-8000-000000000002"))
	want(t, d, "gpt-5.4", 20, true)
}

func TestNullInfoKeepsPercent(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	e.append(`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{}}}` + "\n")
	want(t, d, "gpt-5.4", 20, true)
}

func TestOnlyValidNumbersGivePercent(t *testing.T) {
	cases := []struct {
		name, window, tokens string
		percent              int
		has                  bool
	}{
		{"zero window", "0", "50000", 0, false},
		{"negative window", "-1", "50000", 0, false},
		{"string window", `"250000"`, "50000", 0, false},
		{"negative tokens", "250000", "-1", 0, false},
		{"missing tokens", "250000", "null", 0, false},
		{"empty context", "250000", "0", 0, true},
		{"full context", "250000", "999999", 100, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, fixture(t, "context.jsonl"))
			d := e.detector()
			want(t, d, "gpt-5.4", 20, true)
			e.append(tokenCount(c.window, c.tokens))
			want(t, d, "gpt-5.4", c.percent, c.has)
		})
	}
}

func TestMalformedLineDropsPercent(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	e.append("{broken\n")
	want(t, d, "gpt-5.4", 0, false)
}

func TestTwoCandidateRolloutsAreUnknown(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	other := "019a1234-5678-7000-8000-000000000002"
	e.addRollout(other, strings.ReplaceAll(fixture(t, "context.jsonl"), id, other))
	wantNone(t, e.detector())
}

func TestSubagentRolloutIsNotACandidate(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	other := "019a1234-5678-7000-8000-000000000002"
	sub := strings.ReplaceAll(fixture(t, "context.jsonl"), id, other)
	sub = strings.Replace(sub, `"source":"cli"`, `"source":{"subagent":{}}`, 1)
	e.addRollout(other, sub)
	want(t, e.detector(), "gpt-5.4", 20, true)
}

func TestHeaderMustBeSessionMeta(t *testing.T) {
	lines := strings.SplitAfter(fixture(t, "context.jsonl"), "\n")
	for name, content := range map[string]string{
		"empty":      "",
		"malformed":  "{broken}\n",
		"not meta":   strings.Join(lines[1:], ""),
		"wrong id":   strings.ReplaceAll(lines[0], id, "another-thread") + strings.Join(lines[1:], ""),
		"no newline": strings.TrimSuffix(lines[0], "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			wantNone(t, setup(t, content).detector())
		})
	}
}

func TestRolloutWithoutModelIsUnknown(t *testing.T) {
	lines := strings.SplitAfter(fixture(t, "context.jsonl"), "\n")
	wantNone(t, setup(t, lines[0]).detector())
}

func TestFilesOutsideSessionsAreIgnored(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	e.open = []string{filepath.Join(e.home, "elsewhere", filepath.Base(e.rollup))}
	wantNone(t, e.detector())
}

func TestCodexHomeFromAgentEnv(t *testing.T) {
	e := setup(t, "")
	e.open = nil
	e.codex = filepath.Join(realTemp(t), "custom")
	e.agent = map[string]string{"CODEX_HOME": e.codex}
	e.rollup = e.addRollout(id, fixture(t, "context.jsonl"))
	want(t, e.detector(), "gpt-5.4", 20, true)
}

func TestHostHomeIgnoredWhenCodexHomeSet(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	e.agent = map[string]string{"CODEX_HOME": filepath.Join(realTemp(t), "other")}
	wantNone(t, e.detector())
}

func TestRolloutPathCachedWithinTTLAndRefreshedAfter(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	clock := time.Unix(1000, 0)
	d := e.detector()
	d.(*detector).now = func() time.Time { return clock }
	want(t, d, "gpt-5.4", 20, true)
	e.append(tokenCount("250000", "125000"))
	clock = clock.Add(29 * time.Second)
	want(t, d, "gpt-5.4", 50, true)
	if e.calls != 1 {
		t.Errorf("OpenFiles called %d times, want 1", e.calls)
	}

	// /new: the agent now holds a different rollout, the old file still exists.
	next := "019a1234-5678-7000-8000-000000000003"
	e.open = nil
	e.addRollout(next, strings.ReplaceAll(strings.ReplaceAll(fixture(t, "context.jsonl"), id, next), "gpt-5.4", "gpt-6"))
	clock = clock.Add(2 * time.Second)
	want(t, d, "gpt-6", 20, true)
	if e.calls != 2 {
		t.Errorf("OpenFiles called %d times, want 2", e.calls)
	}
}

func TestFailedRefreshKeepsCachedRollout(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	clock := time.Unix(1000, 0)
	d := e.detector()
	d.(*detector).now = func() time.Time { return clock }
	want(t, d, "gpt-5.4", 20, true)
	e.open = nil
	clock = clock.Add(time.Minute)
	want(t, d, "gpt-5.4", 20, true)
}

func TestSymlinkedHomes(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// lsof reports resolved paths; EvalSymlinks on macOS also resolves /var.
	resolved, err := filepath.EvalSymlinks(filepath.Dir(real))
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		home func(e *env)
		cwd  string
	}{
		"symlinked default":    {home: func(e *env) { e.home = link }},
		"symlinked CODEX_HOME": {env: map[string]string{"CODEX_HOME": link}},
		"relative CODEX_HOME":  {env: map[string]string{"CODEX_HOME": "rel"}, cwd: filepath.Dir(link)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(real, 0o755); err != nil {
				t.Fatal(err)
			}
			if name == "symlinked default" {
				// ~/.codex is the symlink: HOME/.codex -> real
				home := t.TempDir()
				if err := os.Symlink(real, filepath.Join(home, ".codex")); err != nil {
					t.Fatal(err)
				}
				e := &env{t: t, home: home, codex: real}
				e.addRollout(id, fixture(t, "context.jsonl"))
				e.open = []string{filepath.Join(resolved, "real", "sessions", "2026", "10", "05", "rollout-2026-10-05T12-00-00-"+id+".jsonl")}
				want(t, e.detector(), "gpt-5.4", 20, true)
				return
			}
			if name == "relative CODEX_HOME" {
				if err := os.Symlink(real, filepath.Join(filepath.Dir(link), "rel")); err != nil && !os.IsExist(err) {
					t.Fatal(err)
				}
			}
			e := &env{t: t, home: t.TempDir(), codex: real, agent: tc.env}
			e.addRollout(id, fixture(t, "context.jsonl"))
			e.open = []string{filepath.Join(resolved, "real", "sessions", "2026", "10", "05", "rollout-2026-10-05T12-00-00-"+id+".jsonl")}
			p := pane
			p.Cwd = tc.cwd
			got, ok := e.detector().Read(p)
			if !ok || got.Model != "gpt-5.4" {
				t.Errorf("got %+v ok=%v", got, ok)
			}
		})
	}
}

func TestDeletedRolloutDropsCacheAndResolvesAgain(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	if err := os.Remove(e.rollup); err != nil {
		t.Fatal(err)
	}
	wantNone(t, d)
	// The agent moved on to a new rollout.
	next := "019a1234-5678-7000-8000-000000000003"
	e.open = nil
	e.addRollout(next, strings.ReplaceAll(fixture(t, "context.jsonl"), id, next))
	want(t, d, "gpt-5.4", 20, true)
	if e.calls != 2 {
		t.Errorf("OpenFiles called %d times, want 2", e.calls)
	}
}

func TestTruncationDropsDerivedState(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	header := strings.SplitAfter(fixture(t, "context.jsonl"), "\n")[0]
	if err := os.WriteFile(e.rollup, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	wantNone(t, d)
}

func TestPartialLineWaitsForNewline(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	change := fixture(t, "model-change.jsonl")
	e.append(change[:100])
	want(t, d, "gpt-5.4", 20, true)
	e.append(change[100:])
	want(t, d, "gpt-5.4-mini", 0, false)
}

func TestNewPidDropsCache(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	d := e.detector()
	want(t, d, "gpt-5.4", 20, true)
	p := pane
	p.AgentPID = 43
	if _, ok := d.Read(p); !ok {
		t.Fatal("want reading")
	}
	if e.calls != 2 {
		t.Errorf("OpenFiles called %d times, want 2", e.calls)
	}
}

func TestCodexChildHoldingRolloutIsFollowed(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	opened := e.open
	e.open = nil
	d := New(agentctx.Host{
		Home: e.home,
		Env:  func(int) map[string]string { return nil },
		OpenFiles: func(pid int) []string {
			if pid == 99 {
				return opened
			}
			return nil
		},
		Descendants: func(int) []agentctx.Proc {
			return []agentctx.Proc{
				{PID: 98, Argv: []string{"/bin/sh", "-c", "x"}},
				{PID: 99, Argv: []string{"node", "/opt/lib/codex/bin/codex.js"}},
			}
		},
	})
	want(t, d, "gpt-5.4", 20, true)
}

func TestUnresolvedRolloutBacksOffBeforeRetrying(t *testing.T) {
	e := setup(t, fixture(t, "context.jsonl"))
	e.open = nil
	clock := time.Unix(1000, 0)
	d := e.detector()
	d.(*detector).now = func() time.Time { return clock }
	wantNone(t, d)
	for i := 0; i < 4; i++ {
		clock = clock.Add(2 * time.Second)
		wantNone(t, d)
	}
	if e.calls != 1 {
		t.Fatalf("OpenFiles called %d times within the back-off, want 1", e.calls)
	}
	e.open = []string{e.rollup}
	clock = clock.Add(2 * time.Second) // 10s after the failed lookup
	want(t, d, "gpt-5.4", 20, true)
	if e.calls != 2 {
		t.Errorf("OpenFiles called %d times after the back-off, want 2", e.calls)
	}
}
