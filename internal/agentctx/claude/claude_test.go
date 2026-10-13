package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

const (
	cwd = "/home/a/shop"
	sid = "a01c-9f"
)

func assistant(model string, input, create, read int, extra string) string {
	return fmt.Sprintf(`{"type":"assistant"%s,"message":{"model":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":5}}}`,
		extra, model, input, create, read)
}

const (
	userLine = `{"type":"user","message":{"role":"user","content":"hi"}}`
	compact  = `{"type":"system","subtype":"compact_boundary"}`
)

type env struct {
	home  string
	agent map[string]string
	pane  agentctx.Pane
}

func setup(t *testing.T, lines ...string) *env {
	t.Helper()
	e := &env{home: t.TempDir(), pane: agentctx.Pane{ID: "p1", Kind: "claude", SessionID: sid, Cwd: cwd, AgentPID: 42}}
	e.write(t, filepath.Join(e.home, ".claude"), lines...)
	return e
}

func (e *env) write(t *testing.T, dir string, lines ...string) {
	t.Helper()
	p := filepath.Join(dir, "projects", Slug(cwd), sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) detector() agentctx.Detector {
	return New(agentctx.Host{
		Home: e.home,
		Env:  func(pid int) map[string]string { return e.agent },
	})
}

func TestClaudeReadsLastAssistant(t *testing.T) {
	e := setup(t,
		userLine,
		assistant("claude-sonnet-4-5-20250929", 100_000, 0, 70_000, ""),
		assistant("claude-opus-5-5", 100_000, 0, 70_000, ""),
	)
	got, ok := e.detector().Read(e.pane)
	want := agentctx.Reading{Model: "Opus 5.5", Percent: 17, HasPercent: true}
	if !ok || got != want {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, want)
	}
}

func TestClaudeIgnoresSidechainAndSynthetic(t *testing.T) {
	e := setup(t,
		assistant("claude-opus-5-5", 100_000, 0, 0, ""),
		assistant("claude-sonnet-4-5", 1, 0, 0, `,"isSidechain":true`),
		assistant("<synthetic>", 1, 0, 0, ""),
	)
	got, ok := e.detector().Read(e.pane)
	want := agentctx.Reading{Model: "Opus 5.5", Percent: 10, HasPercent: true}
	if !ok || got != want {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, want)
	}
}

func TestClaudeCompactHidesPercent(t *testing.T) {
	e := setup(t, assistant("claude-opus-5-5", 100_000, 0, 0, ""), compact)
	got, ok := e.detector().Read(e.pane)
	want := agentctx.Reading{Model: "Opus 5.5"}
	if !ok || got != want {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, want)
	}
}

func TestClaudeNoReplyYet(t *testing.T) {
	e := setup(t, userLine)
	if got, ok := e.detector().Read(e.pane); ok {
		t.Errorf("got %+v, want unknown", got)
	}
}

func TestClaudeBadSessionID(t *testing.T) {
	e := setup(t, assistant("claude-opus-5-5", 100_000, 0, 0, ""))
	// A file a traversal would reach: <home>/.claude/projects/x.jsonl
	bait := filepath.Join(e.home, ".claude", "projects", "x.jsonl")
	if err := os.WriteFile(bait, []byte(assistant("claude-opus-5-5", 1, 0, 0, "")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.pane.SessionID = "../x"
	if got, ok := e.detector().Read(e.pane); ok {
		t.Errorf("got %+v, want unknown", got)
	}
	e.pane.SessionID = ""
	if _, ok := e.detector().Read(e.pane); ok {
		t.Error("empty session id must be unknown")
	}
}

func TestClaudeConfigDir(t *testing.T) {
	e := setup(t) // nothing under ~/.claude worth reading
	cfg := t.TempDir()
	e.write(t, cfg, assistant("claude-opus-5-5", 200_000, 0, 0, ""))
	e.agent = map[string]string{"CLAUDE_CONFIG_DIR": cfg}
	got, ok := e.detector().Read(e.pane)
	want := agentctx.Reading{Model: "Opus 5.5", Percent: 20, HasPercent: true}
	if !ok || got != want {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, want)
	}
}

func TestClaudeIncrementalAndReset(t *testing.T) {
	e := setup(t, assistant("claude-opus-5-5", 100_000, 0, 0, ""))
	d := e.detector()
	d.Read(e.pane)
	path := filepath.Join(e.home, ".claude", "projects", Slug(cwd), sid+".jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintln(f, assistant("claude-opus-5-5", 300_000, 0, 0, ""))
	f.Close()
	if got, _ := d.Read(e.pane); got.Percent != 30 {
		t.Errorf("after append percent = %d, want 30", got.Percent)
	}
	// Rewrite with no reply: stale cached reply must be dropped.
	e.write(t, filepath.Join(e.home, ".claude"), userLine)
	if got, ok := d.Read(e.pane); ok {
		t.Errorf("after rewrite got %+v, want unknown", got)
	}
}

func TestClaudeSessionChangeStartsFresh(t *testing.T) {
	e := setup(t, assistant("claude-opus-5-5", 100_000, 0, 0, ""))
	d := e.detector()
	d.Read(e.pane)
	e.pane.SessionID = "other-1" // no such transcript
	if got, ok := d.Read(e.pane); ok {
		t.Errorf("got %+v, want unknown for a new session", got)
	}
}

func TestClaudeSettingsRequestWindow(t *testing.T) {
	e := setup(t, assistant("claude-sonnet-4-5-20250929", 100_000, 0, 0, ""))
	e.agent = map[string]string{"ANTHROPIC_MODEL": "sonnet[1m]"}
	got, _ := e.detector().Read(e.pane)
	if got.Percent != 10 {
		t.Errorf("percent = %d, want 10 (1M window via ANTHROPIC_MODEL)", got.Percent)
	}
	e.agent = nil
	e.pane.Argv = []string{"claude", "--model", "sonnet[1m]"}
	if got, _ = e.detector().Read(e.pane); got.Percent != 10 {
		t.Errorf("percent = %d, want 10 via --model", got.Percent)
	}
	e.pane.Argv = nil
	settings := filepath.Join(e.home, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte(`{"model":"sonnet[1m]"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.detector().Read(e.pane); got.Percent != 10 {
		t.Errorf("percent = %d, want 10 via settings", got.Percent)
	}
}

func TestClaudeSettingsReadFromConfigDir(t *testing.T) {
	e := setup(t)
	cfg := t.TempDir()
	e.write(t, cfg, assistant("claude-sonnet-4-5-20250929", 100_000, 0, 0, ""))
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"model":"sonnet[1m]"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.agent = map[string]string{"CLAUDE_CONFIG_DIR": cfg}
	got, ok := e.detector().Read(e.pane)
	if !ok || got.Percent != 10 {
		t.Errorf("got %+v ok=%v, want 10%% (1M window via settings in CLAUDE_CONFIG_DIR)", got, ok)
	}
}
