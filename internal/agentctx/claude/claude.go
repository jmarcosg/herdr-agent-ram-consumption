// Package claude detects the model and context usage of a Claude Code pane by
// tailing its session transcript.
package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/jsonl"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/model"
)

const (
	synthetic = "<synthetic>"
	envConfig = "CLAUDE_CONFIG_DIR"
	envModel  = "ANTHROPIC_MODEL"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

type reply struct {
	model  string
	tokens int
}

type detector struct {
	host      agentctx.Host
	tail      jsonl.Tail
	session   string
	cwd       string
	path      string
	last      *reply
	compacted bool
}

// New returns a Claude Code detector. It keeps incremental state, so use one
// instance per pane.
func New(h agentctx.Host) agentctx.Detector { return &detector{host: h} }

func (d *detector) Read(p agentctx.Pane) (agentctx.Reading, bool) {
	if !safeID.MatchString(p.SessionID) || p.Cwd == "" {
		return agentctx.Reading{}, false
	}
	var agentEnv map[string]string
	if d.host.Env != nil {
		agentEnv = d.host.Env(p.AgentPID)
	}
	dir := agentEnv[envConfig]
	if dir == "" {
		dir = filepath.Join(d.host.Home, ".claude")
	}
	path := filepath.Join(dir, "projects", Slug(p.Cwd), p.SessionID+".jsonl")

	if p.SessionID != d.session || p.Cwd != d.cwd || path != d.path {
		d.tail, d.last, d.compacted = jsonl.Tail{}, nil, false
		d.session, d.cwd, d.path = p.SessionID, p.Cwd, path
	}

	lines, reset, err := d.tail.Read(path)
	if err != nil {
		d.last, d.compacted = nil, false
		return agentctx.Reading{}, false
	}
	if reset {
		d.last, d.compacted = nil, false
	}
	for _, line := range lines {
		d.mark(line)
	}
	if d.last == nil {
		return agentctx.Reading{}, false
	}

	out := agentctx.Reading{Model: model.Pretty(d.last.model)}
	if !d.compacted {
		env, requested := d.limits(agentEnv, p)
		out.Percent = percent(d.last.tokens, Window(d.last.model, d.last.tokens, env, requested))
		out.HasPercent = true
	}
	return out, true
}

type entry struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Sidechain bool   `json:"isSidechain"`
	Message   *struct {
		Model string `json:"model"`
		Usage *struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// mark applies one transcript line: a real assistant reply replaces the cached
// one and a compact boundary hides the percent until the next reply.
func (d *detector) mark(line []byte) {
	isCompact := bytes.Contains(line, []byte(`"compact_boundary"`))
	isReply := bytes.Contains(line, []byte(`"type":"assistant"`)) && bytes.Contains(line, []byte(`"usage"`))
	if !isCompact && !isReply {
		return
	}
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch {
	case e.Type == "system" && e.Subtype == "compact_boundary":
		d.compacted = true
	case e.Type == "assistant" && !e.Sidechain && e.Message != nil && e.Message.Usage != nil:
		m := e.Message
		tokens := m.Usage.Input + m.Usage.CacheCreation + m.Usage.CacheRead
		if m.Model == "" || m.Model == synthetic || tokens <= 0 {
			return
		}
		d.last = &reply{model: m.Model, tokens: tokens}
		d.compacted = false
	}
}

// limits builds the env and requested model Window needs: the agent's env,
// overridden by the "env" blocks of the settings files, and the requested model
// from --model, then ANTHROPIC_MODEL, then settings "model".
func (d *detector) limits(agentEnv map[string]string, p agentctx.Pane) (map[string]string, string) {
	project := filepath.Join(p.Cwd, ".claude")
	paths := []string{
		filepath.Join(d.host.Home, ".claude", "settings.json"),
		filepath.Join(project, "settings.json"),
		filepath.Join(project, "settings.local.json"),
	}
	env := make(map[string]string, len(agentEnv))
	for k, v := range agentEnv {
		env[k] = v
	}
	settingsModel := ""
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s struct {
			Env   map[string]any `json:"env"`
			Model string         `json:"model"`
		}
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		for k, v := range s.Env {
			if str, ok := v.(string); ok {
				env[k] = str
			}
		}
		if s.Model != "" {
			settingsModel = s.Model
		}
	}
	requested := modelFlag(p.Argv)
	if requested == "" {
		requested = strings.TrimSpace(env[envModel])
	}
	if requested == "" {
		requested = settingsModel
	}
	return env, requested
}

// modelFlag returns the last --model value ("--model x" or "--model=x").
func modelFlag(argv []string) string {
	found := ""
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--model" && i+1 < len(argv) {
			i++
			found = argv[i]
		} else if v, ok := strings.CutPrefix(argv[i], "--model="); ok {
			found = v
		}
	}
	return found
}
