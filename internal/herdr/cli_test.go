package herdr

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fake returns a Runner that records argv and replies with out/err.
func fake(out []byte, err error, got *[]string) Runner {
	return func(_ context.Context, bin string, args ...string) ([]byte, error) {
		*got = append([]string{bin}, args...)
		return out, err
	}
}

func TestAgentsParse(t *testing.T) {
	var got []string
	c := New("herdr", fake(fixture(t, "agent-list.json"), nil, &got))
	agents, err := c.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"herdr", "agent", "list"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	want := []Agent{
		{PaneID: "wD:pQ", Kind: "claude", SessionID: "c7e84f4c-fa67-4438-b669-5d8081734400", Cwd: "/Users/test/dev/minimalart"},
		{PaneID: "wD:pY", Kind: "claude", SessionID: "180aaa76-0fea-4386-8955-ad8c10b1f16a", Cwd: "/Users/test/dev/minimalart"},
	}
	if !reflect.DeepEqual(agents, want) {
		t.Errorf("agents = %+v, want %+v", agents, want)
	}
}

func TestProcessInfoParse(t *testing.T) {
	var got []string
	c := New("herdr", fake(fixture(t, "process-info.json"), nil, &got))
	info, err := c.ProcessInfo(context.Background(), "wD:p11")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"herdr", "pane", "process-info", "--pane", "wD:p11"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if info.ShellPID != 35242 || info.ForegroundPGID != 35452 {
		t.Errorf("pids = %d/%d", info.ShellPID, info.ForegroundPGID)
	}
	if len(info.Procs) != 7 {
		t.Fatalf("procs = %d, want 7", len(info.Procs))
	}
	// Procs without argv still carry argv0.
	if p := info.Procs[3]; p.PID != 35595 || p.Argv0 != "mcp@latest --isolated" || p.Argv != nil {
		t.Errorf("argv-less proc = %+v", p)
	}
	last := info.Procs[6]
	if last.PID != 35452 || last.Argv0 != "claude" || !reflect.DeepEqual(last.Argv, []string{"claude"}) || last.Cwd != "/Users/test/dev/minimalart" {
		t.Errorf("claude proc = %+v", last)
	}
}

func TestProcessInfoPaneGone(t *testing.T) {
	var got []string
	body := []byte(`{"error":{"code":"pane_not_found","message":"pane x not found"},"id":"cli:request"}`)
	c := New("herdr", fake(body, errors.New("exit status 1"), &got))
	if _, err := c.ProcessInfo(context.Background(), "x"); !errors.Is(err, ErrPaneGone) {
		t.Errorf("err = %v, want ErrPaneGone", err)
	}
}

func TestReportArgs(t *testing.T) {
	var got []string
	c := New("herdr", fake(nil, nil, &got))
	err := c.ReportTokens(context.Background(), "wD:p1",
		map[string]string{"agent_mem": "1.2 GB"}, []string{"agent_ctx_warn"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"herdr", "pane", "report-metadata", "wD:p1", "--source", "agent-stats",
		"--token", "agent_mem=1.2 GB", "--clear-token", "agent_ctx_warn"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestReportArgsSorted(t *testing.T) {
	var got []string
	c := New("herdr", fake(nil, nil, &got))
	_ = c.ReportTokens(context.Background(), "p",
		map[string]string{"b": "2", "a": "1"}, []string{"z", "y"})
	want := []string{"herdr", "pane", "report-metadata", "p", "--source", "agent-stats",
		"--token", "a=1", "--token", "b=2", "--clear-token", "y", "--clear-token", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestReportPaneGone(t *testing.T) {
	var got []string
	body := []byte(`{"error":{"code":"pane_not_found","message":"pane does-not-exist not found"},"id":"cli:request"}`)
	c := New("herdr", fake(body, errors.New("exit status 1"), &got))
	if err := c.ReportTokens(context.Background(), "does-not-exist", map[string]string{"a": "1"}, nil); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestReportOtherErrorSurfaces(t *testing.T) {
	var got []string
	c := New("herdr", fake(nil, errors.New("exit status 1: boom"), &got))
	if err := c.ReportTokens(context.Background(), "p", map[string]string{"a": "1"}, nil); err == nil {
		t.Error("want error")
	}
}

func TestBinPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := BinPath(env(nil)); got != "herdr" {
		t.Errorf("default = %q", got)
	}
	if got := BinPath(env(map[string]string{"HERDR_BIN_PATH": "/x/herdr"})); got != "/x/herdr" {
		t.Errorf("env = %q", got)
	}
}
