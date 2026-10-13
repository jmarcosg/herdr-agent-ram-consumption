package opencode

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

const modelsJSON = `{"deepseek":{"models":{
  "deepseek-v4-pro":{"name":"DeepSeek V4 Pro","limit":{"context":1000000}},
  "deepseek-v4-flash":{"name":"DeepSeek V4 Flash","limit":{"context":500000}}}}}`

const agentPID = 42

var start = time.UnixMilli(1_000_000)

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type env struct {
	t       *testing.T
	home    string
	project string
	db      string
	models  string
	now     time.Time

	agentEnv map[string]string
	open     map[int][]string // pid -> open files
	descs    []agentctx.Proc
	procs    []agentctx.Proc
	opened   int
	msgSeq   int
	conn     *sql.DB
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, home: realTemp(t), project: realTemp(t), now: time.UnixMilli(5_000_000), agentEnv: map[string]string{}}
	data := filepath.Join(e.home, ".local", "share", "opencode")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	e.db = filepath.Join(data, "opencode.db")
	e.models = filepath.Join(e.home, "models.json")
	if err := os.WriteFile(e.models, []byte(modelsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	e.agentEnv["OPENCODE_MODELS_PATH"] = e.models
	conn, err := sql.Open("sqlite", e.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	e.conn = conn
	e.exec(`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, time_archived INTEGER, time_updated INTEGER);
CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT)`)
	e.open = map[int][]string{agentPID: {e.db}}
	return e
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.conn.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) session(id, dir string, updated int64) {
	e.exec(`INSERT INTO session (id, directory, time_updated) VALUES (?, ?, ?)`, id, dir, updated)
}

func (e *env) message(session, data string) {
	e.msgSeq++
	e.exec(`INSERT INTO message (id, session_id, time_created, data) VALUES (?, ?, ?, ?)`,
		fmt.Sprintf("msg_%04d", e.msgSeq), session, e.msgSeq, data)
}

func user(provider, model string) string {
	return fmt.Sprintf(`{"role":"user","time":{"created":1},"model":{"providerID":%q,"modelID":%q}}`, provider, model)
}

func assistant(provider, model string, input, output, reasoning, cacheRead, cacheWrite int) string {
	return fmt.Sprintf(`{"role":"assistant","providerID":%q,"modelID":%q,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}}}`,
		provider, model, input, output, reasoning, cacheRead, cacheWrite)
}

func (e *env) detector() *detector {
	d := New(agentctx.Host{
		Home: e.home,
		Env: func(int) map[string]string {
			return e.agentEnv
		},
		OpenFiles: func(pid int) []string {
			e.opened++
			return e.open[pid]
		},
		Descendants: func(int) []agentctx.Proc { return e.descs },
		Processes:   func() []agentctx.Proc { return e.procs },
	}).(*detector)
	d.now = func() time.Time { return e.now }
	return d
}

func (e *env) pane() agentctx.Pane {
	return agentctx.Pane{ID: "p1", Kind: "opencode", Cwd: e.project, AgentPID: agentPID, Start: start}
}

func want(t *testing.T, d agentctx.Detector, p agentctx.Pane, model string, percent int, has bool) {
	t.Helper()
	got, ok := d.Read(p)
	w := agentctx.Reading{Model: model, Percent: percent, HasPercent: has}
	if !ok || got != w {
		t.Errorf("got %+v ok=%v, want %+v", got, ok, w)
	}
}

func wantNone(t *testing.T, d agentctx.Detector, p agentctx.Pane) {
	t.Helper()
	if got, ok := d.Read(p); ok {
		t.Errorf("got %+v, want unknown", got)
	}
}

func TestNewestRootSessionForCwdSinceStart(t *testing.T) {
	e := setup(t)
	elsewhere := realTemp(t)
	e.session("ses_old", e.project, 500_000) // before start
	e.message("ses_old", assistant("deepseek", "deepseek-v4-flash", 1, 1, 0, 0, 0))
	e.session("ses_else", elsewhere, 3_000_000)
	e.message("ses_else", assistant("deepseek", "deepseek-v4-flash", 1, 1, 0, 0, 0))
	e.session("ses_older", e.project, 2_000_000)
	e.message("ses_older", assistant("deepseek", "deepseek-v4-flash", 1, 1, 0, 0, 0))
	e.session("ses_new", e.project, 4_000_000)
	e.message("ses_new", user("deepseek", "deepseek-v4-pro"))
	e.message("ses_new", assistant("deepseek", "deepseek-v4-pro", 20_000, 5_000, 5_000, 80_000, 10_000))

	// 120000 / 1000000
	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 12, true)
}

func TestNoSessionSinceStartIsUnknown(t *testing.T) {
	e := setup(t)
	e.session("ses_old", e.project, 500_000)
	e.message("ses_old", user("deepseek", "deepseek-v4-pro"))
	wantNone(t, e.detector(), e.pane())
}

func TestArchivedAndChildSessionsAreIgnored(t *testing.T) {
	e := setup(t)
	e.session("ses_main", e.project, 2_000_000)
	e.message("ses_main", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.session("ses_child", e.project, 3_000_000)
	e.message("ses_child", assistant("deepseek", "deepseek-v4-flash", 1, 1, 0, 0, 0))
	e.exec(`UPDATE session SET parent_id = 'ses_main' WHERE id = 'ses_child'`)
	e.session("ses_archived", e.project, 4_000_000)
	e.message("ses_archived", assistant("deepseek", "deepseek-v4-flash", 1, 1, 0, 0, 0))
	e.exec(`UPDATE session SET time_archived = 1 WHERE id = 'ses_archived'`)

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestModelFromNewestMessageNoPercentWhenReplyUsedAnotherModel(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.message("s", user("deepseek", "deepseek-v4-flash"))

	want(t, e.detector(), e.pane(), "DeepSeek V4 Flash", 0, false)
}

func TestReplyWithoutOutputIsNotAReading(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 900_000, 0, 0, 0, 0)) // still streaming

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestUnknownModelShowsRawIDWithoutPercent(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "my-local-model", 1000, 10, 0, 0, 0))
	want(t, e.detector(), e.pane(), "my-local-model", 0, false)

	if err := os.Remove(e.models); err != nil {
		t.Fatal(err)
	}
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 1000, 10, 0, 0, 0))
	want(t, e.detector(), e.pane(), "deepseek-v4-pro", 0, false)
}

func TestModelsFileFromXDGCacheHome(t *testing.T) {
	e := setup(t)
	delete(e.agentEnv, "OPENCODE_MODELS_PATH")
	cache := realTemp(t)
	e.agentEnv["XDG_CACHE_HOME"] = cache
	if err := os.MkdirAll(filepath.Join(cache, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "opencode", "models.json"), []byte(modelsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-flash", 250_000, 1, 0, 0, 0))

	want(t, e.detector(), e.pane(), "DeepSeek V4 Flash", 50, true)
}

func TestBrokenMessagesAreSkipped(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.message("s", "{broken")
	e.message("s", "[]")

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestSecondOpencodeInSameCwdAndDBIsUnknown(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.procs = []agentctx.Proc{
		{PID: agentPID, Argv: []string{"opencode"}, Cwd: e.project},
		{PID: 99, Argv: []string{"/usr/local/bin/opencode"}, Cwd: e.project},
		{PID: 100, Argv: []string{"opencode"}, Cwd: realTemp(t)}, // other folder
	}
	e.open[99] = []string{e.db}
	d := e.detector()
	wantNone(t, d, e.pane())

	// the other one exits; the next resolution sees a lone opencode
	e.procs = e.procs[:1]
	e.now = e.now.Add(reresolve)
	want(t, d, e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestOpencodeWithOtherDBOrFamilyIsNotShared(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.procs = []agentctx.Proc{
		{PID: 99, Argv: []string{"opencode"}, Cwd: e.project}, // other db
		{PID: 98, Argv: []string{"opencode"}, Cwd: e.project}, // own descendant
	}
	e.open[99] = []string{filepath.Join(e.home, "elsewhere", "opencode.db")}
	e.open[98] = []string{e.db}
	e.descs = []agentctx.Proc{{PID: 98, Argv: []string{"opencode"}, Cwd: e.project}}

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestDescendantHoldingTheDBIsTheHolder(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	e.open = map[int][]string{77: {e.db}}
	e.descs = []agentctx.Proc{
		{PID: 70, Argv: []string{"zsh"}, Cwd: e.project},
		{PID: 77, Argv: []string{"node", "/x/opencode"}, Cwd: e.project},
	}

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)
}

func TestOpencodeDBOverride(t *testing.T) {
	e := setup(t)
	custom := filepath.Join(e.home, "state", "agent.sqlite")
	if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.db, custom); err != nil {
		t.Fatal(err)
	}
	e.agentEnv["OPENCODE_DB"] = custom
	e.open = map[int][]string{agentPID: {custom, e.db + "-wal"}}
	e.conn.Close()
	conn, err := sql.Open("sqlite", custom)
	if err != nil {
		t.Fatal(err)
	}
	e.conn = conn
	t.Cleanup(func() { conn.Close() })
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))

	want(t, e.detector(), e.pane(), "DeepSeek V4 Pro", 10, true)

	// without the override, agent.sqlite is not an opencode database
	delete(e.agentEnv, "OPENCODE_DB")
	wantNone(t, e.detector(), e.pane())
}

func TestIsDatabase(t *testing.T) {
	for _, c := range []struct {
		path, named string
		want        bool
	}{
		{"/h/.local/share/opencode/opencode.db", "", true},
		{"/h/.local/share/opencode/opencode-beta-2.db", "", true},
		{"/h/.local/share/opencode/opencode.db-wal", "", false},
		{"/h/.local/share/other/app.db", "", false},
		{"/srv/state/agent.sqlite", "agent.sqlite", true},
		{"/h/opencode.db", "agent.sqlite", false},
	} {
		if got := isDatabase(c.path, c.named); got != c.want {
			t.Errorf("isDatabase(%q, %q) = %v, want %v", c.path, c.named, got, c.want)
		}
	}
}

func TestNoDatabaseOpenIsUnknown(t *testing.T) {
	e := setup(t)
	e.open = map[int][]string{}
	wantNone(t, e.detector(), e.pane())
}

func TestBrokenSchemaIsUnknown(t *testing.T) {
	e := setup(t)
	e.exec(`DROP TABLE message; DROP TABLE session`)
	wantNone(t, e.detector(), e.pane())
}

func TestDatabaseResolvedOncePerPidAndReresolvedAfterTTL(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	d := e.detector()
	want(t, d, e.pane(), "DeepSeek V4 Pro", 10, true)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 200_000, 1, 0, 0, 0))
	want(t, d, e.pane(), "DeepSeek V4 Pro", 20, true)
	if e.opened != 1 {
		t.Fatalf("OpenFiles called %d times, want 1", e.opened)
	}
	e.now = e.now.Add(reresolve)
	want(t, d, e.pane(), "DeepSeek V4 Pro", 20, true)
	if e.opened != 2 {
		t.Fatalf("OpenFiles called %d times after TTL, want 2", e.opened)
	}
	p := e.pane()
	p.AgentPID = 43
	e.open[43] = []string{e.db}
	want(t, d, p, "DeepSeek V4 Pro", 20, true)
	if e.opened != 3 {
		t.Fatalf("OpenFiles called %d times after pid change, want 3", e.opened)
	}
}

func TestDatabaseGoneIsUnknownAndReresolved(t *testing.T) {
	e := setup(t)
	e.session("s", e.project, 2_000_000)
	e.message("s", assistant("deepseek", "deepseek-v4-pro", 100_000, 1, 0, 0, 0))
	d := e.detector()
	want(t, d, e.pane(), "DeepSeek V4 Pro", 10, true)
	if err := os.Remove(e.db); err != nil {
		t.Fatal(err)
	}
	wantNone(t, d, e.pane())
	if e.opened != 2 {
		t.Fatalf("OpenFiles called %d times, want 2 (re-resolved when the db vanished)", e.opened)
	}
}
