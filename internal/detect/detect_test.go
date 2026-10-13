package detect

import (
	"testing"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

func TestDetectKinds(t *testing.T) {
	for _, k := range []string{"claude", "codex", "opencode", "gemini"} {
		if New(k, agentctx.Host{}) == nil {
			t.Errorf("%s: nil detector", k)
		}
	}
	if New("cursor", agentctx.Host{}) != nil {
		t.Error("cursor: want nil")
	}
}
