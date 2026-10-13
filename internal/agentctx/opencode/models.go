package opencode

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

type modelInfo struct {
	name   string
	window uint64
}

// modelCache remembers the last lookup so models.json (large) is parsed again
// only when the file or the wanted model changes.
type modelCache struct {
	path            string
	mtime           time.Time
	size            int64
	provider, model string
	info            modelInfo
	ok              bool
	valid           bool
}

func (c *modelCache) find(path, provider, model string) (modelInfo, bool) {
	var mtime time.Time
	var size int64
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			mtime, size = st.ModTime(), st.Size()
		} else {
			path = ""
		}
	}
	if c.valid && c.path == path && c.mtime.Equal(mtime) && c.size == size && c.provider == provider && c.model == model {
		return c.info, c.ok
	}
	info, ok := readModel(path, provider, model)
	*c = modelCache{path, mtime, size, provider, model, info, ok, true}
	return info, ok
}

func readModel(path, provider, model string) (modelInfo, bool) {
	if path == "" {
		return modelInfo{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return modelInfo{}, false
	}
	var providers map[string]struct {
		Models map[string]struct {
			Name  string `json:"name"`
			Limit *struct {
				Context uint64 `json:"context"`
			} `json:"limit"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &providers) != nil {
		return modelInfo{}, false
	}
	e, ok := providers[provider].Models[model]
	if !ok {
		return modelInfo{}, false
	}
	info := modelInfo{name: strings.TrimSpace(e.Name)}
	if info.name == "" {
		info.name = model
	}
	if e.Limit != nil {
		info.window = e.Limit.Context
	}
	return info, true
}
