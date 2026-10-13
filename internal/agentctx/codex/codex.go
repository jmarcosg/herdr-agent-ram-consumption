// Package codex detects the model and context usage of a Codex pane by tailing
// the rollout file its process holds open.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/jsonl"
)

const (
	envHome    = "CODEX_HOME"
	maxProcs   = 16
	maxHeader  = 64 * 1024
	idLen      = 36
	stampLen   = 20 // "2026-10-05T12-00-00-"
	rolloutPre = "rollout-"
	rolloutExt = ".jsonl"
)

type detector struct {
	host agentctx.Host

	// Resolved once per agent pid: finding the rollout costs a lsof.
	pid  int
	path string
	id   string

	tail    jsonl.Tail
	model   string
	percent int
	has     bool
}

// New returns a Codex detector. It keeps incremental state, so use one
// instance per pane.
func New(h agentctx.Host) agentctx.Detector { return &detector{host: h} }

func (d *detector) Read(p agentctx.Pane) (agentctx.Reading, bool) {
	if p.AgentPID != d.pid {
		d.forget()
		d.pid = p.AgentPID
	}
	if d.path == "" {
		path, id, ok := d.rolloutPath(p.AgentPID)
		if !ok {
			return agentctx.Reading{}, false
		}
		d.path, d.id = path, id
	}

	lines, reset, err := d.tail.Read(d.path)
	if err != nil {
		d.clearState()
		if errors.Is(err, fs.ErrNotExist) {
			d.path, d.id = "", ""
		}
		return agentctx.Reading{}, false
	}
	if reset {
		d.clearState()
	}
	for _, line := range lines {
		d.apply(line)
	}
	if d.model == "" {
		return agentctx.Reading{}, false
	}
	return agentctx.Reading{Model: d.model, Percent: d.percent, HasPercent: d.has}, true
}

func (d *detector) forget() {
	d.path, d.id = "", ""
	d.tail = jsonl.Tail{}
	d.clearState()
}

func (d *detector) clearState() { d.model, d.percent, d.has = "", 0, false }

func (d *detector) setModel(m string) {
	m = strings.TrimSpace(m)
	if m != d.model {
		d.model = m
		d.percent, d.has = 0, false
	}
}

type entry struct {
	Type    string `json:"type"`
	Payload struct {
		Type     string `json:"type"`
		Model    string `json:"model"`
		ThreadID string `json:"thread_id"`
		Settings struct {
			Model string `json:"model"`
		} `json:"thread_settings"`
		Info *struct {
			Last struct {
				Total any `json:"total_tokens"`
			} `json:"last_token_usage"`
			Window any `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

func (d *detector) apply(line []byte) {
	var e entry
	if json.Unmarshal(line, &e) != nil {
		d.percent, d.has = 0, false
		return
	}
	pl := e.Payload
	switch e.Type {
	case "turn_context":
		d.setModel(pl.Model)
	case "compacted":
		d.percent, d.has = 0, false
	case "event_msg":
		switch pl.Type {
		case "thread_settings_applied":
			if pl.ThreadID == "" || pl.ThreadID == d.id {
				d.setModel(pl.Settings.Model)
			}
		case "token_count":
			if pl.Info == nil { // rate-limit update, not a context reading
				return
			}
			tokens, ok1 := whole(pl.Info.Last.Total)
			window, ok2 := whole(pl.Info.Window)
			d.has = ok1 && ok2 && window > 0
			d.percent = 0
			if d.has {
				d.percent = min((tokens*100+window/2)/window, 100)
			}
		case "context_compacted":
			d.percent, d.has = 0, false
		}
	}
}

// whole returns v as a non-negative integer when it is a JSON number.
func whole(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// rolloutPath finds the single valid rollout open by the agent or by a Codex
// child of it. Zero or several candidates mean the pane is unknown.
func (d *detector) rolloutPath(pid int) (path, id string, ok bool) {
	pids := []int{pid}
	if d.host.Descendants != nil {
		for _, c := range d.host.Descendants(pid) {
			if len(pids) >= maxProcs {
				break
			}
			if isCodex(c.Argv) && !slices.Contains(pids, c.PID) {
				pids = append(pids, c.PID)
			}
		}
	}
	var found []string
	for _, p := range pids {
		sessions := filepath.Join(d.codexHome(p), "sessions") + string(filepath.Separator)
		if d.host.OpenFiles == nil {
			break
		}
		for _, f := range d.host.OpenFiles(p) {
			if strings.HasPrefix(f, sessions) && sessionID(f) != "" && !slices.Contains(found, f) {
				found = append(found, f)
			}
		}
	}
	var valid []string
	for _, f := range found {
		if validHeader(f) {
			valid = append(valid, f)
		}
	}
	if len(valid) != 1 {
		return "", "", false
	}
	return valid[0], sessionID(valid[0]), true
}

func (d *detector) codexHome(pid int) string {
	if d.host.Env != nil {
		if h := d.host.Env(pid)[envHome]; h != "" {
			return h
		}
	}
	return filepath.Join(d.host.Home, ".codex")
}

// isCodex reports whether one of the first two args is the codex binary or its
// node wrapper script.
func isCodex(argv []string) bool {
	for _, a := range argv[:min(2, len(argv))] {
		if filepath.Base(a) == "codex" || strings.HasSuffix(a, "/codex/bin/codex.js") {
			return true
		}
	}
	return false
}

// sessionID extracts the uuid from rollout-<ts>_<uuid>.jsonl, or "".
func sessionID(path string) string {
	name, ok := strings.CutPrefix(filepath.Base(path), rolloutPre)
	if !ok {
		return ""
	}
	name, ok = strings.CutSuffix(name, rolloutExt)
	if !ok || len(name) < stampLen {
		return ""
	}
	id, _, _ := strings.Cut(name[stampLen:], "_")
	if len(id) != idLen {
		return ""
	}
	for _, c := range id {
		if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	return id
}

// validHeader requires a complete first line that is a session_meta from an
// interactive source (not a subagent) whose id matches the file name.
func validHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, maxHeader)).ReadBytes('\n')
	if err != nil {
		return false
	}
	var h struct {
		Type    string `json:"type"`
		Payload struct {
			ID     string `json:"id"`
			Source any    `json:"source"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &h) != nil || h.Type != "session_meta" {
		return false
	}
	src, _ := h.Payload.Source.(string)
	return (src == "cli" || src == "exec" || src == "vscode") && h.Payload.ID == sessionID(path)
}
