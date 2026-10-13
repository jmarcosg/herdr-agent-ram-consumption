// Package gemini detects the model and context usage of a Gemini CLI pane by
// tailing the newest main chat file of the agent's project.
package gemini

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
	"time"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/jsonl"
)

const (
	envHome      = "GEMINI_CLI_HOME"
	rescan       = 30 * time.Second // the chat can change under a live process
	retryAfter   = 10 * time.Second // back-off after a scan that found nothing
	maxHeader    = 64 * 1024
	maxReplies   = 512
	window       = 1_048_576
	gemmaWindow  = 256_000
	setPrefix    = "Model set to "
	persisted    = " (persisted)"
	chatExt      = ".jsonl"
	maxPercent   = 100
	gemmaPrefix  = "gemma-4"
	projectsFile = "projects.json"
)

type reply struct {
	id      string
	model   string
	input   int64
	hasIn   bool
	written time.Time
}

type picked struct {
	model string
	at    time.Time
}

type detector struct {
	host agentctx.Host
	now  func() time.Time // injectable for tests

	// Choosing the chat costs a directory scan and a process listing, so the
	// result is cached per agent pid.
	pid      int
	path     string
	scanned  time.Time
	nextScan time.Time

	tail    jsonl.Tail
	replies []reply
	model   string // last model seen on any reply
	picked  *picked
}

// New returns a Gemini detector. It keeps incremental state, so use one
// instance per pane.
func New(h agentctx.Host) agentctx.Detector { return &detector{host: h, now: time.Now} }

func (d *detector) Read(p agentctx.Pane) (agentctx.Reading, bool) {
	if p.AgentPID != d.pid {
		d.forget()
		d.pid = p.AgentPID
	}
	if d.path == "" || d.now().Sub(d.scanned) >= rescan {
		if d.path == "" && d.now().Before(d.nextScan) {
			return agentctx.Reading{}, false
		}
		path, ok := d.chatPath(p)
		if !ok {
			d.forget()
			d.nextScan = d.now().Add(retryAfter)
			return agentctx.Reading{}, false
		}
		if path != d.path {
			d.forget()
			d.path = path
		}
		d.scanned = d.now()
	}

	lines, reset, err := d.tail.Read(d.path)
	if err != nil {
		d.clearState()
		if errors.Is(err, fs.ErrNotExist) {
			d.path = ""
			d.nextScan = time.Time{}
		}
		return agentctx.Reading{}, false
	}
	if reset {
		d.clearState()
		if !validHeader(d.path) { // replaced by something that is not a main chat
			d.forget()
			d.nextScan = d.now().Add(retryAfter)
			return agentctx.Reading{}, false
		}
	}
	for _, line := range lines {
		d.apply(line)
	}
	return d.reading()
}

func (d *detector) forget() {
	d.path = ""
	d.scanned = time.Time{}
	d.tail = jsonl.Tail{}
	d.clearState()
}

func (d *detector) clearState() {
	d.replies, d.model, d.picked = nil, "", nil
}

type entry struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Content   any    `json:"content"`
	Model     string `json:"model"`
	Timestamp string `json:"timestamp"`
	Tokens    struct {
		Input any `json:"input"`
	} `json:"tokens"`
	Set *struct {
		Messages json.RawMessage `json:"messages"`
	} `json:"$set"`
}

func (d *detector) apply(line []byte) {
	var e entry
	if json.Unmarshal(line, &e) != nil {
		d.replies = nil
		return
	}
	if e.Set != nil {
		if len(e.Set.Messages) == 0 {
			return
		}
		var msgs []entry
		if json.Unmarshal(e.Set.Messages, &msgs) != nil {
			return
		}
		d.replies = nil
		for _, m := range msgs {
			d.message(m)
		}
		return
	}
	d.message(e)
}

func (d *detector) message(e entry) {
	if e.ID == "" {
		return
	}
	at, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
	if e.Type == "info" {
		if text, ok := e.Content.(string); ok {
			if m, ok := strings.CutPrefix(text, setPrefix); ok {
				if at.IsZero() {
					at = d.now()
				}
				d.picked = &picked{model: strings.TrimSuffix(m, persisted), at: at}
			}
		}
		return
	}
	if e.Type != "gemini" {
		return
	}
	r := reply{id: e.ID, written: at}
	if m := strings.TrimSpace(e.Model); m != "" {
		r.model = m
		d.model = m
	}
	if f, ok := e.Tokens.Input.(float64); ok && f >= 0 && f == float64(int64(f)) {
		r.input, r.hasIn = int64(f), true
	}
	if i := slices.IndexFunc(d.replies, func(o reply) bool { return o.id == r.id }); i >= 0 {
		d.replies[i] = r
		return
	}
	d.replies = append(d.replies, r)
	if len(d.replies) > maxReplies {
		d.replies = d.replies[1:]
	}
}

func (d *detector) reading() (agentctx.Reading, bool) {
	var last *reply
	for i := len(d.replies) - 1; i >= 0; i-- {
		if d.replies[i].hasIn {
			last = &d.replies[i]
			break
		}
	}
	model := ""
	if last != nil {
		model = last.model
	}
	// A model chosen after the last reply replaces it until the new model answers.
	if pk := d.picked; pk != nil && pk.model != model && (last == nil || last.written.IsZero() || last.written.Before(pk.at)) {
		return agentctx.Reading{Model: pk.model}, true
	}
	if model == "" {
		if d.model == "" {
			return agentctx.Reading{}, false
		}
		return agentctx.Reading{Model: d.model}, true
	}
	w := int64(window)
	if strings.HasPrefix(model, gemmaPrefix) {
		w = gemmaWindow
	}
	return agentctx.Reading{Model: model, Percent: int(min((last.input*100+w/2)/w, maxPercent)), HasPercent: true}, true
}

// chatPath finds the newest main chat of the agent's project, or reports false
// when the project is unknown or another gemini shares the same cwd and home.
func (d *detector) chatPath(p agentctx.Pane) (string, bool) {
	holder, cwd := d.holder(p)
	cwd = realPath(cwd)
	if cwd == "" {
		return "", false
	}
	dir := d.geminiHome(holder, cwd)
	if d.sharedWith(holder, p.AgentPID, cwd, dir) {
		return "", false
	}
	slug := projectSlug(dir, cwd)
	if slug == "" {
		return "", false
	}
	project := filepath.Join(dir, "tmp", slug)
	if owner, err := os.ReadFile(filepath.Join(project, ".project_root")); err == nil {
		if realPath(strings.TrimSpace(string(owner))) != cwd {
			return "", false
		}
	}
	entries, err := os.ReadDir(filepath.Join(project, "chats"))
	if err != nil {
		return "", false
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if filepath.Ext(e.Name()) != chatExt || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(p.Start) {
			continue
		}
		path := filepath.Join(project, "chats", e.Name())
		if !validHeader(path) {
			continue
		}
		if m := info.ModTime(); best == "" || m.After(bestTime) || m.Equal(bestTime) && path > best {
			best, bestTime = path, m
		}
	}
	return best, best != ""
}

// holder is the gemini process the pane runs: the deepest gemini descendant of
// the agent, else the agent itself (the wrapper may be a node launcher).
func (d *detector) holder(p agentctx.Pane) (pid int, cwd string) {
	pid, cwd = p.AgentPID, p.Cwd
	if d.host.Descendants == nil {
		return
	}
	for _, c := range d.host.Descendants(p.AgentPID) {
		if isGemini(c.Argv) {
			pid, cwd = c.PID, c.Cwd
		}
	}
	return
}

// geminiHome is <GEMINI_CLI_HOME or $HOME>/.gemini of the process, symlinks
// resolved so it compares equal across processes.
func (d *detector) geminiHome(pid int, cwd string) string {
	home := d.host.Home
	if d.host.Env != nil {
		if h := d.host.Env(pid)[envHome]; h != "" {
			home = h
			if !filepath.IsAbs(home) && cwd != "" {
				home = filepath.Join(cwd, home)
			}
		}
	}
	dir := filepath.Join(home, ".gemini")
	if real := realPath(dir); real != "" {
		return real
	}
	return filepath.Clean(dir)
}

// sharedWith reports whether another gemini runs on the same cwd and home, in
// which case the newest chat may belong to it.
func (d *detector) sharedWith(holder, agent int, cwd, dir string) bool {
	if d.host.Processes == nil {
		return false
	}
	family := []int{holder, agent}
	if d.host.Descendants != nil {
		for _, c := range d.host.Descendants(agent) {
			family = append(family, c.PID)
		}
	}
	for _, o := range d.host.Processes() {
		if slices.Contains(family, o.PID) || !isGemini(o.Argv) || realPath(o.Cwd) != cwd {
			continue
		}
		if d.geminiHome(o.PID, o.Cwd) == dir {
			return true
		}
	}
	return false
}

// projectSlug maps cwd to its slug in projects.json, or "" when unknown or
// unsafe to use as a path component.
func projectSlug(dir, cwd string) string {
	b, err := os.ReadFile(filepath.Join(dir, projectsFile))
	if err != nil {
		return ""
	}
	var f struct {
		Projects map[string]string `json:"projects"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	for path, slug := range f.Projects {
		if realPath(path) != cwd {
			continue
		}
		if slug == "" || slug == "." || slug == ".." || strings.ContainsAny(slug, `/\`) {
			return ""
		}
		return slug
	}
	return ""
}

func realPath(p string) string {
	if p == "" {
		return ""
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

// isGemini reports whether one of the first two args is the gemini binary or
// the script a node launcher runs.
func isGemini(argv []string) bool {
	for _, a := range argv[:min(2, len(argv))] {
		if filepath.Base(a) == "gemini" {
			return true
		}
	}
	return false
}

// validHeader requires a complete first line that is a main (not subagent) chat
// with a well-formed session id.
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
		SessionID string `json:"sessionId"`
		Kind      string `json:"kind"`
	}
	if json.Unmarshal(line, &h) != nil || h.Kind != "main" || h.SessionID == "" {
		return false
	}
	for _, c := range h.SessionID {
		if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
