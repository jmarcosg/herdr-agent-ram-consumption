// Package detect maps a herdr agent kind to its context detector.
package detect

import (
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx/claude"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx/codex"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx/gemini"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx/opencode"
)

// New returns the detector for kind, or nil when the kind is unsupported.
func New(kind string, h agentctx.Host) agentctx.Detector {
	switch kind {
	case "claude":
		return claude.New(h)
	case "codex":
		return codex.New(h)
	case "opencode":
		return opencode.New(h)
	case "gemini":
		return gemini.New(h)
	}
	return nil
}
