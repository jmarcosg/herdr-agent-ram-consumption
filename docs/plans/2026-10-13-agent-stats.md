# herdr agent stats Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A herdr plugin that writes `model · context% · RAM` tokens for every agent pane, plus a herdr-radar PR that renders them as an extra sidebar row.

**Architecture:** A Go daemon started by herdr (`[[startup]]`) polls `herdr agent list` + `herdr pane process-info` every 2 s, measures the pane's process tree footprint, reads each agent's session files for model and token usage, and sends only changed tokens with `herdr pane report-metadata`. Algorithms are ported from Cornercase.

**Tech Stack:** Go 1.27 (cgo on darwin for libproc), `modernc.org/sqlite` (OpenCode DB, pure Go), standard `testing`. Radar side: Node, `node --test`.

**Spec:** `docs/specs/2026-10-13-agent-stats-design.md`

**Reference source:** Cornercase `usecornercase/cornercase-terminal` @ `17890687ac1c3639f38c921039f4a22106d2b5cc` (Apache-2.0). "Port from X" below means: reproduce that function's behaviour exactly, including its test cases in the sibling `tests.rs`, adapted to Go.

## Global Constraints

- Module `github.com/jmarcosg/herdr-agent-ram-consumption`; binary `herdr-agent-stats`; plugin id `jmarcosg.agent-stats`; `min_herdr_version = "0.9.3"`; platforms `["linux", "macos"]`.
- Token source string: `agent-stats`. Token names: `agent_model`, `agent_ctx_ok`, `agent_ctx_warn`, `agent_ctx_crit`, `agent_mem`. At most one `agent_ctx_*` is set at a time.
- Severity: `< 75` → ok, `75–89` → warn, `>= 90` → crit.
- Poll interval 2 s. Descendant cap 256. JSONL tail window 1 MiB (`1 << 20`).
- herdr binary from `HERDR_BIN_PATH`, default `herdr`. State dir from `HERDR_PLUGIN_STATE_DIR`, default `~/.local/state/herdr/plugins/jmarcosg.agent-stats`.
- Unknown values are cleared (`--clear-token`), never written empty. Only diffs are sent.
- A `pane_not_found` reply from herdr counts as success.
- Commits: conventional commits, no AI attribution / Co-Authored-By lines.
- Do not run `go build` as a verification step; `go test` / `go vet` only.
- Repo ships `NOTICE` crediting Cornercase (Apache-2.0) and `LICENSE` (Apache-2.0).

## Review Focus

1. **Fresh agent with no reply yet** — no assistant line in the transcript: model/ctx tokens cleared, `agent_mem` still set. Test in Task 9 (`TestTokensRAMOnly`) and Task 5 (`TestClaudeNoReplyYet`).
2. **One huge JSONL line (> 1 MiB tool output)** — reader skips it without error and resumes on the next newline. Test in Task 4 (`TestTailSkipsOversizedLine`).
3. **Agent restarted in the same pane** (new pid or new kind) — previous model/percent never shown for the new agent. Test in Task 9 (`TestResetOnAgentChange`).
4. **Pane closed between list and report / herdr not running** — `pane_not_found` is success; any other error is logged and the loop continues next tick. Test in Task 8 (`TestReportPaneGone`) and Task 9 (`TestTickSurvivesHerdrError`).
5. **Two OpenCode or Gemini agents in the same cwd** — detector returns unknown instead of the other agent's model. Tests in Tasks 7a/7b (ported `shared` cases).

---

## File Structure

```
cmd/herdr-agent-stats/main.go       subcommands: daemon, stop, nudge, once
internal/format/format.go           Size, Severity
internal/model/model.go             Pretty (claude id → "Opus 5.5")
internal/process/process.go         shared Descendants
internal/process/process_darwin.go  cgo libproc: Children, Footprint, StartTime, Env, OpenFiles
internal/process/process_linux.go   /proc equivalents
internal/memory/memory.go           Measure
internal/jsonl/tail.go              incremental tail reader
internal/agentctx/agentctx.go       Pane, Reading, Detector, Host
internal/agentctx/claude/           Claude detector
internal/agentctx/codex/            Codex detector
internal/agentctx/opencode/         OpenCode detector
internal/agentctx/gemini/           Gemini detector
internal/detect/detect.go           New(kind, host) Detector factory
internal/herdr/cli.go               herdr CLI client
internal/daemon/daemon.go           Tick, Clear, Tokens, AgentPID, diffing
herdr-plugin.toml, README.md, LICENSE, NOTICE
```

---

### Task 1: Scaffold + formatters

**Files:**
- Create: `go.mod`, `.gitignore` (`/herdr-agent-stats`), `LICENSE` (Apache-2.0 text), `NOTICE`, `internal/format/format.go`
- Test: `internal/format/format_test.go`

**Interfaces:**
- Produces: `format.Size(bytes uint64) string`, `format.Severity(percent int) string` (returns `"ok"|"warn"|"crit"`), `const format.MB = 1 << 20`, `format.GB = 1 << 30`.

- [ ] **Step 1: Write the failing tests**

```go
func TestSize(t *testing.T) {
	cases := map[uint64]string{
		0: "0 MB", 300 * MB: "300 MB", 300*MB + MB/2: "301 MB",
		1023 * MB: "1023 MB", 1024*MB - 1: "1.0 GB",
		GB + GB/5: "1.2 GB", GB + GB/4: "1.3 GB", 10 * GB: "10.0 GB",
	}
	// assert format.Size(k) == v for each
}
func TestSeverity(t *testing.T) {
	// 0,74 → "ok"; 75,89 → "warn"; 90,100 → "crit"
}
```

- [ ] **Step 2: Run** `go test ./internal/format/` → FAIL (undefined).
- [ ] **Step 3: Implement** — `mb := (b + MB/2) / MB`; if `mb < 1024` → `"%d MB"`; else `tenths := (b*10 + GB/2) / GB` → `"%d.%d GB"`. `NOTICE`: "This product includes algorithms ported from Cornercase (https://github.com/usecornercase/cornercase-terminal), Copyright the Cornercase authors, licensed under the Apache License 2.0."
- [ ] **Step 4: Run** `go test ./internal/format/` → PASS.
- [ ] **Step 5: Commit** `git add -A && git commit -m "feat: scaffold module and size/severity formatters"`

---

### Task 2: Model name prettifier

**Files:** Create `internal/model/model.go`; Test `internal/model/model_test.go`

**Interfaces:**
- Produces: `model.Pretty(id string) string`.

- [ ] **Step 1: Write the failing test** (table):

| in | out |
|---|---|
| `claude-opus-5-5` | `Opus 5.5` |
| `claude-opus-5-5[1m]` | `Opus 5.5` |
| `claude-sonnet-4-5-20250929` | `Sonnet 4.5` |
| `claude-sonnet-4-20250514` | `Sonnet 4` |
| `claude-opus-4-0` | `Opus 4` |
| `claude-3-5-sonnet-20241022` | `Sonnet 3.5` |
| `anthropic.claude-haiku-4-5@20251001` | `Haiku 4.5` |
| `vertex/claude-opus-4-1` | `Opus 4.1` |
| `gpt-5-codex` | `gpt-5-codex` |
| `` | `` |

- [ ] **Step 2: Run** `go test ./internal/model/` → FAIL.
- [ ] **Step 3: Implement** — port `Model::parse` / `name` from Cornercase `src/context.rs` (≈L576-613): lowercase, last `/` segment, strip `anthropic.` prefix, cut at `@` or `[`, require `claude-` prefix; split on `-`; family = first all-alpha token; version = tokens of 1–2 digits; output `Title(family) + " " + join(version, ".")`, `[major, 0]` → `major`. Anything unparseable → original input unchanged.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: prettify claude model ids`

---

### Task 3: Process inspection + memory

**Files:**
- Create: `internal/process/process.go`, `process_darwin.go` (`//go:build darwin`, cgo `#include <libproc.h>`), `process_linux.go` (`//go:build linux`), `internal/memory/memory.go`
- Test: `internal/process/process_test.go`, `internal/memory/memory_test.go`

**Interfaces:**
- Produces:
  - `process.Children(pid int) []int`
  - `process.Descendants(pid, max int) []int` — BFS over `Children`, excludes `pid`, stops at `max`.
  - `process.Footprint(pid int) (uint64, bool)` — false for dead/inaccessible.
  - `process.StartTime(pid int) (time.Time, bool)`
  - `process.Env(pid int) map[string]string` — nil when unreadable.
  - `process.OpenFiles(pid int) []string` — absolute paths of open regular files.
  - `process.Argv(pid int) []string` — nil when unreadable (darwin: same `kern.procargs2` buffer as `Env`; linux: `/proc/<pid>/cmdline`).
  - `process.Cwd(pid int) string` — "" when unreadable (darwin: `proc_pidinfo(PROC_PIDVNODEPATHINFO)`; linux: readlink `/proc/<pid>/cwd`).
  - `memory.Measure(root int) (uint64, bool)` — `Footprint(root)` + `Footprint` of `Descendants(root, 256)`, saturating add, false only if nothing measured.

- [ ] **Step 1: Write the failing tests** (run against the test binary itself):
  - `TestFootprintSelf`: `Footprint(os.Getpid())` → ok, `> 0`.
  - `TestFootprintDead`: start `sleep 0`, `Wait()`, then `Footprint(pid)` → `false`.
  - `TestChildrenAndDescendants`: start `sh -c "sleep 5 & wait"`; within 1 s `Descendants(os.Getpid(), 256)` contains the `sh` pid and at least one more pid; `Descendants(os.Getpid(), 1)` has length 1. Kill after.
  - `TestStartTime`: `StartTime(os.Getpid())` within 1 min before `time.Now()`.
  - `TestEnv`: run `env FOO_AGENT_STATS=1 sleep 5`; `Env(pid)["FOO_AGENT_STATS"] == "1"`.
  - `TestArgvCwd`: start `sleep 5` with `cmd.Dir = t.TempDir()`; `Argv(pid) == ["sleep","5"]`; `Cwd(pid)` equals the symlink-resolved temp dir.
  - `TestOpenFiles`: open a temp file in the test, `OpenFiles(os.Getpid())` contains its (symlink-resolved) path.
  - `TestMeasure`: `memory.Measure(os.Getpid())` ≥ `Footprint(os.Getpid())`; `Measure(dead pid)` → false.
- [ ] **Step 2: Run** `go test ./internal/process/ ./internal/memory/` → FAIL.
- [ ] **Step 3: Implement darwin**: `proc_listchildpids` (buffer 1024 pids, drop ≤ 0); `proc_pid_rusage(pid, RUSAGE_INFO_V0)` → `ri_phys_footprint`; `proc_pidinfo(PROC_PIDTBSDINFO)` → `pbi_start_tvsec/usec`; `Env` via `unix.SysctlRaw("kern.procargs2", pid)` (layout: int32 argc, exec path, NULs, argc args, then `KEY=VAL` strings); `OpenFiles` via `lsof -n -P -a -p PID -Fn` (lines starting `n/`), 2 s timeout.
- [ ] **Step 4: Implement linux**: `/proc/<pid>/task/*/children` sorted + dedup; footprint `(RssAnon + VmSwap) kB * 1024` from `/proc/<pid>/status` (missing `RssAnon` → false); start = boot time (`/proc/stat` `btime`) + field 22 of `/proc/<pid>/stat` / `CLK_TCK` (100); `/proc/<pid>/environ`; `/proc/<pid>/fd/*` readlink.
- [ ] **Step 5: Run** `go test ./internal/process/ ./internal/memory/` → PASS. Also `GOOS=linux go vet ./internal/process/`.
- [ ] **Step 6: Commit** `feat: measure process tree footprint`

---

### Task 4: Incremental JSONL tail

**Files:** Create `internal/jsonl/tail.go`; Test `internal/jsonl/tail_test.go`

**Interfaces:**
- Produces: `type Tail struct{...}`; `func (t *Tail) Read(path string) (lines [][]byte, reset bool, err error)`; `const Window = 1 << 20`. `reset` is true on the first read and whenever state was discarded (caller must drop what it derived). Missing file → `os.ErrNotExist` wrapped, state cleared.

- [ ] **Step 1: Write the failing tests**:
  - `TestTailFirstReadOnlyComplete`: file `a\nb\nc` → lines `[a b]`, reset true; append `d\n` → `[cd]`, reset false.
  - `TestTailWindow`: file of 3 MiB of 100-byte lines → first read returns only lines from the last 1 MiB, first returned line is complete (partial first line dropped).
  - `TestTailTruncateResets`: shrink the file → reset true, reads from the start.
  - `TestTailRotateResets`: replace the file (new inode) → reset true.
  - `TestTailSameSizeMtimeResets`: rewrite same size with new mtime → reset true.
  - `TestTailSkipsOversizedLine`: 2 MiB line without newline, then `x\n` appended → no error; eventually returns `[x]` and never the oversized line.
- [ ] **Step 2: Run** `go test ./internal/jsonl/` → FAIL.
- [ ] **Step 3: Implement** — port `Tail` from Cornercase `src/jsonl.rs`: track `{inode, size, mtime, offset, skipping}`; read at most `Window` bytes per call; consume up to the last `\n`; when a full window holds no `\n`, set `skipping` and discard until the next `\n`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: incremental jsonl tail reader`

---

### Task 5: Detector contract + Claude detector

**Files:**
- Create: `internal/agentctx/agentctx.go`, `internal/agentctx/claude/claude.go`, `internal/agentctx/claude/slug.go`, `internal/agentctx/claude/window.go`
- Test: `internal/agentctx/claude/*_test.go`, fixtures in `internal/agentctx/claude/testdata/`

**Interfaces:**
- Produces (agentctx):

```go
type Pane struct {
	ID, Kind, SessionID, Cwd string
	AgentPID                 int
	Argv                     []string  // agent process argv
	Start                    time.Time // agent process start
}
type Reading struct {
	Model      string // display-ready
	Percent    int
	HasPercent bool
}
type Detector interface{ Read(p Pane) (Reading, bool) } // false = unknown
type Host struct {
	Home        string                       // $HOME
	Getenv      func(string) string          // this process's env
	Env         func(pid int) map[string]string
	OpenFiles   func(pid int) []string
	Descendants func(pid int) []Proc
}
type Proc struct{ PID int; Argv []string; Cwd string }
```

- Produces (claude): `claude.New(h agentctx.Host) agentctx.Detector`; `claude.Slug(cwd string) string`; `claude.Window(model string, tokens int, env map[string]string, requested string) int`.

- [ ] **Step 1: Write failing slug tests**: `Slug("/Users/jmarcosg/dev/minimalart") == "-Users-jmarcosg-dev-minimalart"`; `Slug("/tmp/añ b")` == `"-tmp-a--b"`; a 250-char path → length ≤ 200 + 1 + len(hash) and equal to the value Cornercase's test produces for the same input (port the long-path case from `src/context/tests.rs`).
- [ ] **Step 2: Write failing window tests**:
  - `claude-opus-5-5`, 100k → 1_000_000
  - `claude-sonnet-4-5-20250929`, 150k → 200_000; 300k → 1_000_000
  - `claude-sonnet-4-5-20250929`, 50k, requested `sonnet[1m]` → 1_000_000
  - `claude-opus-5-5`, env `CLAUDE_CODE_DISABLE_1M_CONTEXT=1` → 200_000
  - any model, env `CLAUDE_CODE_MAX_CONTEXT_TOKENS=500000` → 500_000
- [ ] **Step 3: Write failing detector tests** (fixture `$HOME/.claude/projects/<slug>/<id>.jsonl` under `t.TempDir()`):
  - `TestClaudeReadsLastAssistant`: two assistant lines (sonnet-4-5 then opus-5-5, usage input 100k + cache_read 70k) → `{Model:"Opus 5.5", Percent:17, HasPercent:true}`.
  - `TestClaudeIgnoresSidechainAndSynthetic`: last line is `isSidechain:true` / model `<synthetic>` → previous real reply wins.
  - `TestClaudeCompactHidesPercent`: reply then `compact_boundary` → model kept, `HasPercent:false`.
  - `TestClaudeNoReplyYet`: only user lines → `false`.
  - `TestClaudeBadSessionID`: `../x` → `false`, no file access outside projects dir.
  - `TestClaudeConfigDir`: `CLAUDE_CONFIG_DIR` set in the agent's `Env` → transcript read from there.
- [ ] **Step 4: Run** `go test ./internal/agentctx/...` → FAIL.
- [ ] **Step 5: Implement** — port `project_dir` (`src/context.rs` ≈L285-295), `Limits::window`/`percent` and `Limits::read` (≈L496-509, settings from `~/.claude/settings.json`, `<cwd>/.claude/settings.json`, `<cwd>/.claude/settings.local.json`; requested model from `--model` in `Argv`, then `ANTHROPIC_MODEL`, then settings `model`), and `mark` (≈L474-485). Detector keeps one `jsonl.Tail` and the last reply; `reset` from the tail clears the reply. Tokens = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`, must be > 0. Percent = round(tokens*100/window), capped at 100. Model through `model.Pretty`.
- [ ] **Step 6: Run** → PASS.
- [ ] **Step 7: Commit** `feat: claude code model and context detector`

---

### Task 6: Codex detector

**Files:** Create `internal/agentctx/codex/codex.go`; Test `codex_test.go` + `testdata/`

**Interfaces:**
- Consumes: `agentctx.*`, `jsonl.Tail`.
- Produces: `codex.New(h agentctx.Host) agentctx.Detector`.

- [ ] **Step 1: Write failing tests** — port the cases in Cornercase `src/context/codex/tests.rs`, with `Host.OpenFiles`/`Descendants` faked. Must include: single valid rollout → model from `turn_context.payload.model`; `thread_settings_applied` switches model and drops the percent until the next `token_count`; percent = `last_token_usage.total_tokens / model_context_window`; two candidate rollouts → `false`; first line not `session_meta` → `false`; `CODEX_HOME` from agent env honoured; rollout path resolved once per pid (fake `OpenFiles` call count == 1 across two `Read`s).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — port `rollout_path` (`src/context/codex.rs` ≈L13-37) and the line handling; cache the path keyed by `AgentPID`; model shown raw.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: codex model and context detector`

---

### Task 7a: OpenCode detector

**Files:** Create `internal/agentctx/opencode/opencode.go`; Test `opencode_test.go` (builds a fixture SQLite DB in `t.TempDir()`)

**Interfaces:**
- Produces: `opencode.New(h agentctx.Host) agentctx.Detector`.
- Adds dependency `modernc.org/sqlite`.

- [ ] **Step 1: Write failing tests** — port `src/context/opencode/tests.rs`. Must include: newest root session for the cwd updated since `Start`; model from newest message with `providerID/modelID`, display name from fixture `models.json` (`OPENCODE_MODELS_PATH`), raw id when missing; percent only when the newest assistant reply used the current model; archived/child sessions ignored; second opencode process sharing cwd + DB (fake `Descendants`) → `false`; `OPENCODE_DB` override.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — port `database()`, `shared()`, the two queries (read-only DSN `file:<path>?mode=ro&_pragma=busy_timeout(100)`), and message parsing from `src/context/opencode.rs`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: opencode model and context detector`

### Task 7b: Gemini detector

**Files:** Create `internal/agentctx/gemini/gemini.go`; Test `gemini_test.go` + `testdata/`

**Interfaces:**
- Produces: `gemini.New(h agentctx.Host) agentctx.Detector`.

- [ ] **Step 1: Write failing tests** — port `src/context/gemini/tests.rs` **except** footer parsing (out of scope). Must include: `projects.json` cwd → slug; newest chat with mtime ≥ `Start` and first line `{kind:"main"}`; `$set.messages` rewrite; `Model set to X` info line; percent `tokens.input / 1_048_576`, `gemma-4*` → `256_000`; another gemini on same cwd + home → `false`; invalid slug (`..`) → `false`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** — port from `src/context/gemini.rs`, skipping `footer_model`. Model shown raw.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: gemini model and context detector`

---

### Task 8: herdr client + detector factory

**Files:**
- Create: `internal/herdr/cli.go`, `internal/detect/detect.go`
- Test: `internal/herdr/cli_test.go` (fake `Runner`, JSON fixtures in `testdata/` captured from real `herdr agent list` / `herdr pane process-info --pane` output), `internal/detect/detect_test.go`

**Interfaces:**
- Produces:

```go
type Runner func(ctx context.Context, bin string, args ...string) ([]byte, error)
type Agent struct{ PaneID, Kind, SessionID, Cwd string }
type Proc struct{ PID int; Argv0 string; Argv []string; Cwd string }
type ProcessInfo struct{ ShellPID, ForegroundPGID int; Procs []Proc }
func New(bin string, run Runner) *CLI
func BinPath(getenv func(string) string) string
func (c *CLI) Agents(ctx context.Context) ([]Agent, error)            // agent list → result.agents[]: pane_id, agent, agent_session.value, cwd
func (c *CLI) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) // process-info --pane
func (c *CLI) ReportTokens(ctx context.Context, paneID string, set map[string]string, clear []string) error
var ErrPaneGone = errors.New("pane gone")
```

  and `detect.New(kind string, h agentctx.Host) agentctx.Detector` → claude/codex/opencode/gemini or `nil`.

- [ ] **Step 1: Write failing tests**:
  - `TestAgentsParse`, `TestProcessInfoParse` against fixtures (process-info fixture has `argv` missing on some procs — `Argv0` still parsed).
  - `TestReportArgs`: set `{agent_mem:"1.2 GB"}`, clear `[agent_ctx_warn]` → runner receives `pane report-metadata <id> --source agent-stats --token agent_mem=1.2 GB --clear-token agent_ctx_warn` (set sorted by name, then clears sorted).
  - `TestReportPaneGone`: runner returns error JSON with code `pane_not_found` → `ReportTokens` returns `nil`.
  - `TestDetectKinds`: `claude|codex|opencode|gemini` non-nil; `cursor` nil.
- [ ] **Step 2: Run** `go test ./internal/herdr/ ./internal/detect/` → FAIL.
- [ ] **Step 3: Implement** — mirror `~/dev/herdr-plane/internal/herdr/cli.go` (`call`, envelope, `fromAPI`); `ProcessInfo` returns `ErrPaneGone` on `pane_not_found`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: herdr cli client and detector factory`

---

### Task 9: Daemon core

**Files:** Create `internal/daemon/daemon.go`; Test `internal/daemon/daemon_test.go`

**Interfaces:**
- Consumes: Tasks 1, 3, 5–8.
- Produces:

```go
type Herdr interface {
	Agents(ctx context.Context) ([]herdr.Agent, error)
	ProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	ReportTokens(ctx context.Context, paneID string, set map[string]string, clear []string) error
}
type Deps struct {
	Herdr   Herdr
	Detect  func(kind string) agentctx.Detector
	Measure func(root int) (uint64, bool)
	Start   func(pid int) (time.Time, bool)
	Log     func(format string, args ...any)
}
func New(d Deps) *Daemon
func (d *Daemon) Tick(ctx context.Context) error   // one poll + report
func (d *Daemon) Clear(ctx context.Context) error  // clear every token ever set
func Tokens(r agentctx.Reading, known bool, mem uint64, memOK bool) map[string]string
func AgentPID(kind string, info herdr.ProcessInfo) int
```

- [ ] **Step 1: Write failing tests** (fake Herdr records calls):
  - `TestTokensFull`: `{Opus 5.5, 17, true}`, mem 1.2 GB → `{agent_model:"Opus 5.5", agent_ctx_ok:"17%", agent_mem:"1.2 GB"}`; percent 80 → `agent_ctx_warn`; 95 → `agent_ctx_crit`.
  - `TestTokensRAMOnly`: `known=false` → only `agent_mem`.
  - `TestAgentPID`: claude pane fixture → pid of proc with `Argv0` basename `claude` (35452 in fixture), not the MCP children; no match → `ForegroundPGID`.
  - `TestTickSendsDiffOnly`: two ticks with identical readings → second tick makes zero `ReportTokens` calls; percent 17→80 → one call setting `agent_ctx_warn`, clearing `agent_ctx_ok`.
  - `TestResetOnAgentChange`: tick with pid A (claude, Opus) → tick with pid B, detector returns unknown → `agent_model` and `agent_ctx_*` cleared; a fresh detector instance was requested for B.
  - `TestForgetsClosedPanes`: pane disappears from `Agents` → no report for it, state dropped.
  - `TestTickSurvivesHerdrError`: `Agents` errors → `Tick` returns error, next `Tick` with a working fake reports normally.
  - `TestClear`: after ticks on two panes, `Clear` sends clears for every token set on each.
- [ ] **Step 2: Run** `go test ./internal/daemon/` → FAIL.
- [ ] **Step 3: Implement** — per pane keep `{kind, pid, detector, last map[string]string}`; RAM root is `ShellPID`; `Pane.Start` from `Deps.Start(pid)`; agent argv/cwd from the matched `Proc`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat: daemon poll loop with token diffing`

---

### Task 10: CLI, lifecycle, plugin manifest

**Files:**
- Create: `cmd/herdr-agent-stats/main.go`, `herdr-plugin.toml`, `README.md`
- Test: `cmd/herdr-agent-stats/main_test.go`

**Interfaces:**
- Consumes: Task 9 `daemon.New/Tick/Clear`, Task 8 `herdr.New/BinPath/ExecRunner`.
- Produces subcommands:
  - `daemon` — take `flock` on `<state>/daemon.lock` (exit 0 silently if held), write pid to `<state>/daemon.pid`, `Tick` every 2 s, `SIGUSR1` → immediate tick, `SIGTERM`/`SIGINT` → `Clear` then exit.
  - `nudge` — send `SIGUSR1` to the pid in `daemon.pid`; no-op if absent.
  - `stop` — `SIGTERM` to that pid, wait ≤ 3 s for the lock to free.
  - `once` — one poll without reporting; prints `[{pane, model, percent, mem}]` as JSON.

- [ ] **Step 1: Write failing tests**: `TestStateDir` (env set → used; unset → `~/.local/state/herdr/plugins/jmarcosg.agent-stats`); `TestSecondDaemonExits` (lock held by test → `runDaemon` returns nil immediately).
- [ ] **Step 2: Run** `go test ./cmd/...` → FAIL.
- [ ] **Step 3: Implement** main + manifest:

```toml
id = "jmarcosg.agent-stats"
name = "Agent Stats"
version = "0.1.0"
min_herdr_version = "0.9.3"
description = "Model, context usage and RAM for every agent in the Agents sidebar."
platforms = ["linux", "macos"]

[[build]]
command = ["go", "build", "-o", "herdr-agent-stats", "./cmd/herdr-agent-stats"]

[[startup]]
command = ["./herdr-agent-stats", "daemon"]

[[events]]
on = "pane.agent_detected"
command = ["./herdr-agent-stats", "nudge"]

[[actions]]
id = "restart"
title = "Agent stats: restart daemon"
command = ["sh", "-c", "./herdr-agent-stats stop; ./herdr-agent-stats daemon &"]

[[actions]]
id = "stop"
title = "Agent stats: stop and clear"
command = ["./herdr-agent-stats", "stop"]
```

  `main` wires `agentctx.Host{Home: os.UserHomeDir(), Getenv: os.Getenv, Env: process.Env, OpenFiles: process.OpenFiles, Descendants: pid → process.Descendants(pid, 256) mapped to Proc{PID, process.Argv, process.Cwd}}` and `daemon.Deps{Detect: kind → detect.New(kind, host), Measure: memory.Measure, Start: process.StartTime, Log: log.Printf to <state>/daemon.log}`.
  README: what it shows, install, the radar `[extra_row]` snippet from the spec, credit to Cornercase.
- [ ] **Step 4: Run** `go test ./... && go vet ./...` → PASS.
- [ ] **Step 5: Manual check** (with herdr running): `go run ./cmd/herdr-agent-stats once` → JSON lists this Claude pane with `"model":"Opus 5.5"` and a non-zero `mem`.
- [ ] **Step 6: Commit** `feat: daemon cli and herdr plugin manifest`

---

### Task 11: herdr-radar `[extra_row]` (fork + PR)

Separate repo. Fork `hhdebb/herdr-radar` → `jmarcosg/herdr-radar`, clone to `~/dev/herdr-radar`, branch `feat/extra-row`.

**Files:**
- Modify: `lib/config.js` (export `extraRow()`), `lib/managed-config.js` (`sidebarBlock`, `agentRow`)
- Test: `test/extra-row.test.js`
- Modify: `README.md` (document the table)

**Interfaces:**
- Produces: `config.extraRow(): Array<{token: string, fg: string}>` — from `raw.extra_row` (object) in insertion order; skip names not `^[a-z0-9_]+$` or colours not `^#[0-9a-fA-F]{6}$`; truncate to 16.

- [ ] **Step 1: Write failing tests** (`node:test`, set `HERDR_PLUGIN_CONFIG_DIR` to a temp dir with a `config.toml` before requiring modules; clear `require.cache` between cases):
  - no `[extra_row]` → `sidebarBlock('dark')` output identical to current output (snapshot both).
  - `[extra_row]` with `agent_model`, `agent_mem` → every agent row list (`rows` and each `rows_by_agent` entry) has, immediately before `["$gap"]`, a row `[{ token = "$agent_model", fg = "#8b8e9c", ... }, { token = "$agent_mem", ... }]`.
  - invalid entries (`Bad-Name`, `fg = "red"`) skipped; 20 entries → 16.
  - `managed.foreignTables(text)` on the produced config → `[]`.
- [ ] **Step 2: Run** `npm test` → new tests FAIL.
- [ ] **Step 3: Implement** — in `agentRow`, build the extra row with the existing `cell(token, fg)` helper and splice it before the `"[\"$gap\"]"` row; nothing emitted when `extraRow()` is empty.
- [ ] **Step 4: Run** `npm test && npm run check && npm run format` → PASS.
- [ ] **Step 5: Commit** `feat: extra_row setting for third-party sidebar tokens`
- [ ] **Step 6: Ask the user** before pushing the fork and opening the PR upstream (outward-facing). PR body: motivation (plugins publishing tokens can't add rows without making the sidebar table foreign), config example, test notes.
