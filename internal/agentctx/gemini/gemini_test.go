package gemini

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type env struct {
	t         *testing.T
	home      string // $HOME
	gemini    string // the .gemini directory
	cwd       string
	chats     string
	chat      string
	agentEnv  map[string]string
	others    []agentctx.Proc
	scans     int
	clock     time.Time
	otherEnvs map[int]map[string]string
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, content string) *env {
	t.Helper()
	e := &env{t: t, home: realTemp(t), cwd: realTemp(t), clock: time.Now()}
	e.gemini = filepath.Join(e.home, ".gemini")
	e.register("project")
	e.chats = filepath.Join(e.gemini, "tmp", "project", "chats")
	e.chat = e.addChat("session-a.jsonl", content, time.Now())
	return e
}

func (e *env) register(slug string) {
	e.t.Helper()
	writeFile(e.t, filepath.Join(e.gemini, "projects.json"), `{"projects":{"`+e.cwd+`":"`+slug+`"}}`)
}

func (e *env) addChat(name, content string, mtime time.Time) string {
	e.t.Helper()
	p := filepath.Join(e.chats, name)
	writeFile(e.t, p, content)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) append(content string) {
	e.t.Helper()
	f, err := os.OpenFile(e.chat, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) detector() agentctx.Detector {
	d := New(agentctx.Host{
		Home: e.home,
		Env: func(pid int) map[string]string {
			if m, ok := e.otherEnvs[pid]; ok {
				return m
			}
			return e.agentEnv
		},
		Descendants: func(int) []agentctx.Proc { return nil },
		Processes: func() []agentctx.Proc {
			e.scans++
			return e.others
		},
	}).(*detector)
	d.now = func() time.Time { return e.clock }
	return d
}

func (e *env) pane() agentctx.Pane {
	return agentctx.Pane{ID: "p1", Kind: "gemini", AgentPID: 42, Cwd: e.cwd, Start: time.Now().Add(-time.Hour)}
}

func want(t *testing.T, e *env, d agentctx.Detector, model string, percent int, has bool) {
	t.Helper()
	got, ok := d.Read(e.pane())
	w := agentctx.Reading{Model: model, Percent: percent, HasPercent: has}
	if !ok || got != w {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, w)
	}
}

func wantNone(t *testing.T, e *env, d agentctx.Detector) {
	t.Helper()
	if got, ok := d.Read(e.pane()); ok {
		t.Errorf("got %+v, want unknown", got)
	}
}

func TestInitialChatIsUnknown(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl"))
	wantNone(t, e, e.detector())
}

func TestReplyGivesModelAndPercent(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	want(t, e, e.detector(), "gemini-3.8-flash", 1, true)
}

func TestLatestUpdateOfAMessageWins(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	e.append(strings.Replace(fixture(t, "reply.jsonl"), "10486", "524288", 1))
	want(t, e, d, "gemini-3.8-flash", 50, true)
}

func TestSetMessagesRewriteDropsUsageKeepsModel(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	e.append(fixture(t, "compacted.jsonl"))
	want(t, e, d, "gemini-3.8-flash", 0, false)
}

func TestSetMessagesRewriteReplacesReplies(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl"))
	d := e.detector()
	wantNone(t, e, d)
	rewrite := `{"$set":{"messages":[{"id":"m1","type":"gemini","model":"gemini-3.8-pro","tokens":{"input":524288}}]}}` + "\n"
	e.append(rewrite)
	want(t, e, d, "gemini-3.8-pro", 50, true)
}

func TestModelSwitchShowsNewModelUntilItReplies(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	e.append(fixture(t, "switch.jsonl"))
	want(t, e, d, "gemma-4-31b-it", 0, false)
	// gemma-4 has a 256_000 window.
	e.append(`{"id":"r2","timestamp":"2099-01-01T00:00:00.000Z","type":"gemini","model":"gemma-4-31b-it","tokens":{"input":128000}}` + "\n")
	want(t, e, d, "gemma-4-31b-it", 50, true)
}

func TestPersistedSuffixIsStripped(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	e.append(strings.Replace(fixture(t, "switch.jsonl"), "gemma-4-31b-it", "gemini-3.5-pro (persisted)", 1))
	want(t, e, e.detector(), "gemini-3.5-pro", 0, false)
}

func TestPercentIsCappedAt100(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+strings.Replace(fixture(t, "reply.jsonl"), "10486", "9999999", 1))
	want(t, e, e.detector(), "gemini-3.8-flash", 100, true)
}

func TestPartialLineWaitsForNewline(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl"))
	d := e.detector()
	reply := strings.TrimSuffix(fixture(t, "reply.jsonl"), "\n")
	e.append(reply)
	wantNone(t, e, d)
	e.append("\n")
	want(t, e, d, "gemini-3.8-flash", 1, true)
}

func TestTruncationStartsOver(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	writeFile(t, e.chat, fixture(t, "initial.jsonl"))
	wantNone(t, e, d)
}

func TestNewestMainChatSinceStartIsChosen(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	now := time.Now()
	mid := strings.Replace(fixture(t, "reply.jsonl"), "10486", "524288", 1)
	e.addChat("session-mid.jsonl", fixture(t, "initial.jsonl")+mid, now.Add(-time.Minute))
	// Stale: modified before the agent started.
	e.addChat("session-old.jsonl", fixture(t, "initial.jsonl"), now.Add(-3*time.Hour))
	// Newer but a subagent, so skipped.
	e.addChat("session-sub.jsonl", strings.Replace(fixture(t, "initial.jsonl"), `"main"`, `"subagent"`, 1), now.Add(time.Minute))
	// Newer still but without a header.
	e.addChat("session-bad.jsonl", "not json\n", now.Add(2*time.Minute))
	e.addChat("notes.txt", fixture(t, "initial.jsonl"), now.Add(3*time.Minute))
	// session-a (now) is newer than session-mid.
	want(t, e, e.detector(), "gemini-3.8-flash", 1, true)
	if err := os.Remove(e.chat); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Second)
	want(t, e, e.detector(), "gemini-3.8-flash", 50, true)
}

func TestStaleChatBeforeStartIsIgnored(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(e.chat, old, old); err != nil {
		t.Fatal(err)
	}
	wantNone(t, e, e.detector())
}

func TestAnotherGeminiOnSameCwdAndHomeIsUnknown(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	e.others = []agentctx.Proc{{PID: 99, Argv: []string{"/usr/local/bin/gemini"}, Cwd: e.cwd}}
	wantNone(t, e, e.detector())
}

func TestGeminiElsewhereDoesNotMatter(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	other := realTemp(t)
	e.others = []agentctx.Proc{
		{PID: 99, Argv: []string{"gemini"}, Cwd: other},                // other cwd
		{PID: 98, Argv: []string{"gemini"}, Cwd: e.cwd},                // other home
		{PID: 97, Argv: []string{"/bin/zsh"}, Cwd: e.cwd},              // not gemini
		{PID: 42, Argv: []string{"node", "/x/bin/gemini"}, Cwd: e.cwd}, // the agent itself
	}
	e.otherEnvs = map[int]map[string]string{98: {"GEMINI_CLI_HOME": realTemp(t)}}
	want(t, e, e.detector(), "gemini-3.8-flash", 1, true)
}

func TestNilProcessesMeansNotShared(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := New(agentctx.Host{Home: e.home})
	want(t, e, d, "gemini-3.8-flash", 1, true)
}

func TestGeminiCLIHomeFromAgentEnv(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl"))
	custom := realTemp(t)
	e.agentEnv = map[string]string{"GEMINI_CLI_HOME": custom}
	wantNone(t, e, e.detector()) // nothing under the custom home yet
	e.gemini = filepath.Join(custom, ".gemini")
	e.register("project")
	e.chats = filepath.Join(e.gemini, "tmp", "project", "chats")
	e.chat = e.addChat("session-a.jsonl", fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"), time.Now())
	want(t, e, e.detector(), "gemini-3.8-flash", 1, true)
}

func TestInvalidSlugIsUnknown(t *testing.T) {
	for _, slug := range []string{"..", ".", "", "a/b", "../project"} {
		e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
		e.register(slug)
		wantNone(t, e, e.detector())
	}
}

func TestUnregisteredCwdIsUnknown(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	writeFile(t, filepath.Join(e.gemini, "projects.json"), `{"projects":{"/nowhere":"project"}}`)
	wantNone(t, e, e.detector())
}

func TestProjectRootMustMatchCwd(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	root := filepath.Join(e.gemini, "tmp", "project", ".project_root")
	writeFile(t, root, realTemp(t)+"\n")
	wantNone(t, e, e.detector())
	writeFile(t, root, e.cwd+"\n")
	want(t, e, e.detector(), "gemini-3.8-flash", 1, true)
}

func TestSymlinkedCwdMatches(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	link := filepath.Join(realTemp(t), "link")
	if err := os.Symlink(e.cwd, link); err != nil {
		t.Fatal(err)
	}
	p := e.pane()
	p.Cwd = link
	if _, ok := e.detector().Read(p); !ok {
		t.Error("symlinked cwd should resolve")
	}
}

func TestFailedScanBacksOffTenSeconds(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl"))
	if err := os.Remove(e.chat); err != nil {
		t.Fatal(err)
	}
	d := e.detector()
	wantNone(t, e, d)
	e.clock = e.clock.Add(9 * time.Second)
	wantNone(t, e, d)
	if e.scans != 1 {
		t.Fatalf("scans = %d, want 1 inside the back-off", e.scans)
	}
	e.clock = e.clock.Add(time.Second)
	e.chat = e.addChat("session-b.jsonl", fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"), time.Now())
	want(t, e, d, "gemini-3.8-flash", 1, true)
	if e.scans != 2 {
		t.Fatalf("scans = %d, want 2 after the back-off", e.scans)
	}
}

func TestChosenChatIsCachedAndRescannedEvery30s(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	want(t, e, d, "gemini-3.8-flash", 1, true)
	if e.scans != 1 {
		t.Fatalf("scans = %d, want 1 (cached)", e.scans)
	}
	// A newer chat appears (e.g. /resume in another file); picked up after 30 s.
	newer := strings.Replace(fixture(t, "reply.jsonl"), "10486", "524288", 1)
	e.addChat("session-b.jsonl", fixture(t, "initial.jsonl")+newer, time.Now().Add(time.Minute))
	e.clock = e.clock.Add(29 * time.Second)
	want(t, e, d, "gemini-3.8-flash", 1, true)
	e.clock = e.clock.Add(time.Second)
	want(t, e, d, "gemini-3.8-flash", 50, true)
	if e.scans != 2 {
		t.Fatalf("scans = %d, want 2", e.scans)
	}
}

func TestVanishedChatTriggersRescan(t *testing.T) {
	e := setup(t, fixture(t, "initial.jsonl")+fixture(t, "reply.jsonl"))
	d := e.detector()
	want(t, e, d, "gemini-3.8-flash", 1, true)
	if err := os.Remove(e.chat); err != nil {
		t.Fatal(err)
	}
	wantNone(t, e, d)
	e.chat = e.addChat("session-b.jsonl", fixture(t, "initial.jsonl")+strings.Replace(fixture(t, "reply.jsonl"), "10486", "524288", 1), time.Now())
	e.clock = e.clock.Add(10 * time.Second)
	want(t, e, d, "gemini-3.8-flash", 50, true)
}
