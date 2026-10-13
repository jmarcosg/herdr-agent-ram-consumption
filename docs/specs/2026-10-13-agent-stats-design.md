# herdr-agent-ram-consumption — design

Date: 2026-10-13
Status: draft, pending review

## Goal

Show, for every agent in herdr's Agents sidebar, the model it is using, how full its
context window is, and how much RAM its pane consumes — the way Cornercase does:

```
Opus 5.5 · 17% · 1.2 GB
```

Supported agents from v1: Claude Code, Codex, OpenCode, Gemini CLI.

## Prior art

Ported from Cornercase (`usecornercase/cornercase-terminal`, Rust, Apache-2.0). The
algorithms below follow its `src/process.rs`, `src/memory.rs`, `src/context/*.rs` and
`src/ui/workspaces.rs`. The repo ships a `NOTICE` crediting Cornercase.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Packaging | Standalone herdr plugin, separate from herdr-plane | Unrelated concern |
| Language | Go | Same stack as herdr-plane; cgo gives `proc_pid_rusage` directly |
| Agents | Claude, Codex, OpenCode, Gemini from v1 | User decision |
| Sidebar integration | PR to herdr-radar adding an `extra_rows` extension point (fork until merged) | Radar owns `[ui.sidebar.agents]`; a hand-written table makes radar drop its whole sidebar block (`lib/managed-config.js:77,244,630`) |

## Architecture

```
herdr ──[[startup]]──▶ herdr-agent-stats daemon (single instance, lock file)
                         │ every 2s
                         ├─ herdr agent list            → panes, agent kind, session id, cwd
                         ├─ herdr pane process-info     → shell_pid, agent pid
                         ├─ memory.Measure(shell_pid)   → bytes
                         ├─ context.<agent>.Read(...)   → model, percent
                         └─ pane.report_metadata (diff only, source "agent-stats")
```

Packages:

- `internal/herdr` — socket/CLI client: agent list, process-info, report-metadata.
- `internal/process` — `Children(pid)`, `Footprint(pid)`, `Descendants(pid)` with
  build-tagged `darwin` (cgo, libproc) and `linux` (`/proc`) implementations.
- `internal/memory` — `Measure(root)` plus per-pane cache, one measurement in flight.
- `internal/jsonl` — incremental tail reader (shared by Claude and Codex).
- `internal/context/{claude,codex,opencode,gemini}` — one detector per agent behind a
  common `Detector` interface: `Read(pane) (Reading, error)` with
  `Reading{Model string; Percent *int}`.
- `internal/model` — name prettifier (`claude-opus-5-5` → `Opus 5.5`).
- `internal/daemon` — loop, diffing, token writes, lifecycle.
- `cmd/herdr-agent-stats` — `daemon`, `stop`, `once` (debug: print readings as JSON).

## RAM

- Root = the pane's **shell pid**, not the agent pid: covers shell, agent and every
  MCP child. Breadth-first descendants, capped at 256.
- Metric = private footprint, not RSS:
  - macOS: `proc_pid_rusage(RUSAGE_INFO_V0).ri_phys_footprint` (what Activity Monitor shows).
    Children via `proc_listchildpids`.
  - Linux: `(RssAnon + VmSwap) kB * 1024` from `/proc/<pid>/status`. Children via
    `/proc/<pid>/task/*/children`.
- Dead / inaccessible pids are skipped (footprint returns none).
- Format: binary units labelled MB/GB. `< 1024 MB` → `"300 MB"`; otherwise one decimal
  `"1.2 GB"`. Rounded to nearest.

## Model and context % per agent

### Claude Code

- Session: `herdr agent list` already gives `agent_session.value` (session id).
  Validate `[A-Za-z0-9-]+`.
- Transcript: `$CLAUDE_CONFIG_DIR or ~/.claude` + `/projects/<slug(cwd)>/<id>.jsonl`.
  Slug: every non-ASCII-alphanumeric UTF-16 unit → `-`; if > 200 chars, truncate to 200
  and append `-<base36(JS 31*h hash of cwd)>`.
- Read: tail last 1 MiB on first read, complete lines only; reset on inode change,
  shrink, or same-size mtime change.
- Model: last `type:"assistant"` line with `usage`, not `isSidechain`, model not
  `<synthetic>`. A later `compact_boundary` hides the percent until the next reply.
- Tokens: `input + cache_creation_input + cache_read_input`.
- Window: 1M if model is native long (major ≥ 5, or opus ≥ 4.7) or the session asked
  for `[1m]` (`--model`, `ANTHROPIC_MODEL`, settings `model`); else 1M if tokens > 200k;
  else 200k. Honour `CLAUDE_CODE_DISABLE_1M_CONTEXT`, `CLAUDE_CODE_MAX_CONTEXT_TOKENS`.
  Env read from the agent process.

### Codex

- Rollout: open files of the codex process (macOS `lsof -n -P -a -p PID -Fn`, Linux
  `/proc/<pid>/fd`) under `$CODEX_HOME/sessions` (default `~/.codex`) matching
  `rollout-<ts>_<uuid>.jsonl`; first line must be `session_meta`. Exactly one candidate.
  `lsof` is slow: resolve once per agent pid and cache the path.
- Model: `turn_context.payload.model`, overridden by
  `event_msg/thread_settings_applied.thread_settings.model`. Shown raw.
- Percent: `last_token_usage.total_tokens / model_context_window`; reset on model change.

### OpenCode

- DB: the opencode process (or opencode descendant) holding an `opencode*.db` open
  (`OPENCODE_DB` overrides). Open read-only, 100 ms busy timeout.
- Session: newest non-archived root session updated since agent start whose directory
  equals the process cwd. If another opencode shares cwd + DB → unknown.
- Model: newest message with `providerID/modelID`. Display name from
  `models.json` (`$OPENCODE_MODELS_PATH`, `$XDG_CACHE_HOME/opencode`, `~/.cache/opencode`),
  else raw id.
- Percent: newest assistant `tokens.input+output+reasoning+cache.read+cache.write` over
  `limit.context`, only if that reply used the current model.

### Gemini CLI

- Session: `~/.gemini/projects.json` maps cwd → slug; newest
  `~/.gemini/tmp/<slug>/chats/*.jsonl` with mtime ≥ agent start whose first line is
  `{kind:"main"}`. Another gemini on the same cwd → unknown.
- Model: latest `type:"gemini"` entry's `model`; `"Model set to X"` info lines update it.
  (Cornercase also parses the screen footer — skipped in v1, we would need `pane read`.)
- Percent: `tokens.input / 1_048_576` (`256_000` for `gemma-4*`).

## Display

Three tokens per pane, written with source `agent-stats`:

| Token | Value | Notes |
|---|---|---|
| `$agent_model` | `Opus 5.5` | |
| `$agent_ctx_ok` / `$agent_ctx_warn` / `$agent_ctx_crit` | `17%` | Only one is set; the name carries the colour (< 75 / 75–89 / ≥ 90) because herdr row styles are static |
| `$agent_mem` | `1.2 GB` | |

herdr joins adjacent cells with `·`, so the row renders `Opus 5.5 · 17% · 1.2 GB`.
Unknown fields are cleared, not written empty. Only changed tokens are sent.

The row itself comes from radar's new `[extra_row]` table (see below), in radar's plugin
`config.toml`:

```toml
[extra_row]
agent_model = "#8b8e9c"
agent_ctx_ok = "#8b8e9c"
agent_ctx_warn = "#c78a1f"
agent_ctx_crit = "#c04a4a"
agent_mem = "#8b8e9c"
```

## herdr-radar change (separate PR)

Radar parses its own `config.toml` with a line-based parser that only understands
scalar values (`lib/config.js:35`), so the extension point is a flat table, not an array
of inline tables: `[extra_row]`, keys = token names (without `$`), values = `#rrggbb`,
in file order (JS preserves string-key insertion order). `sidebarBlock()` inserts that
row just before the trailing `["$gap"]` row in `rows` and in every `rows_by_agent`
entry. Invalid names (`^[a-z0-9_]+$`) or colours (`^#[0-9a-fA-F]{6}$`) are skipped; more
than `ROW_TOKEN_LIMIT = 16` entries are truncated to 16. No table → block unchanged.
Until merged, install radar from the fork `jmarcosg/herdr-radar`.

## Lifecycle

- `[[startup]]` runs `herdr-agent-stats daemon`; a lock file under the plugin state dir
  keeps one instance. `[[events]] pane.agent_detected` nudges an immediate refresh.
- Per-pane state resets when the agent pid or kind changes (guards pid reuse).
- Only panes listed by `herdr agent list` are tracked (the Agents sidebar shows nothing
  else). Agents other than the four get RAM only.
- `stop` action: stop daemon and clear every token it wrote.

## Out of scope (v1)

- Gemini screen-footer model detection.
- Subagent token stats.
- Windows.

## Testing

- Unit: model prettifier, slug + hash, size formatter, severity buckets, JSONL tail
  (partial lines, rotation, shrink), each detector against fixture files / a fixture
  SQLite DB.
- Process: build-tagged tests against the test process itself (`os.Getpid()` has a
  non-zero footprint and spawned children appear as descendants).
- Daemon: fake herdr client, assert token diffs.
