// Package opencode detects the model and context usage of an OpenCode pane by
// querying the SQLite database the process holds open.
package opencode

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/agentctx"
)

const (
	name      = "opencode"
	maxProcs  = 16
	messages  = 200
	reresolve = 30 * time.Second
)

type detector struct {
	host agentctx.Host
	now  func() time.Time // injectable for tests

	// Resolved once per agent pid: finding the database costs a lsof, and
	// checking for sibling opencode processes costs a scan of every process.
	pid        int
	resolved   bool
	db         string // database path
	cwd        string // holder's cwd, symlinks resolved
	modelsPath string
	shared     bool
	resolvedAt time.Time

	models modelCache
}

// New returns an OpenCode detector. Use one instance per pane.
func New(h agentctx.Host) agentctx.Detector { return &detector{host: h, now: time.Now} }

func (d *detector) Read(p agentctx.Pane) (agentctx.Reading, bool) {
	if p.AgentPID != d.pid {
		d.pid = p.AgentPID
		d.resolved = false
	}
	if d.resolved {
		if _, err := os.Stat(d.db); err != nil || d.now().Sub(d.resolvedAt) >= reresolve {
			d.resolved = false
		}
	}
	if !d.resolved {
		if !d.resolve(p) {
			return agentctx.Reading{}, false
		}
	}
	if d.shared {
		return agentctx.Reading{}, false
	}
	return d.read(p.Start)
}

// resolve finds the database holder and everything derived from it.
func (d *detector) resolve(p agentctx.Pane) bool {
	holder, db, ok := d.database(p)
	if !ok {
		return false
	}
	cwd, err := filepath.EvalSymlinks(holder.Cwd)
	if err != nil {
		return false
	}
	d.db, d.cwd, d.resolved, d.resolvedAt = db, cwd, true, d.now()
	d.modelsPath = d.modelsFile(holder)
	d.shared = d.isShared(holder.PID, db, cwd)
	return true
}

// database finds the agent, or an opencode descendant, holding an opencode
// database open.
func (d *detector) database(p agentctx.Pane) (agentctx.Proc, string, bool) {
	if d.host.OpenFiles == nil {
		return agentctx.Proc{}, "", false
	}
	cands := []agentctx.Proc{{PID: p.AgentPID, Argv: p.Argv, Cwd: p.Cwd}}
	if d.host.Descendants != nil {
		for _, c := range d.host.Descendants(p.AgentPID) {
			if len(cands) >= maxProcs {
				break
			}
			if isOpencode(c.Argv) {
				cands = append(cands, c)
			}
		}
	}
	for _, c := range cands {
		var named string
		if d.host.Env != nil {
			if v := d.host.Env(c.PID)["OPENCODE_DB"]; v != "" {
				named = filepath.Base(v)
			}
		}
		for _, f := range d.host.OpenFiles(c.PID) {
			if isDatabase(f, named) {
				return c, f, true
			}
		}
	}
	return agentctx.Proc{}, "", false
}

// isShared reports another opencode, outside the holder's own process tree,
// with the same folder and database: its sessions would be indistinguishable.
func (d *detector) isShared(holder int, db, cwd string) bool {
	if d.host.Processes == nil {
		return false
	}
	var family []agentctx.Proc
	if d.host.Descendants != nil {
		family = d.host.Descendants(holder)
	}
	for _, o := range d.host.Processes() {
		if o.PID == holder || !isOpencode(o.Argv) ||
			slices.ContainsFunc(family, func(f agentctx.Proc) bool { return f.PID == o.PID }) {
			continue
		}
		if real, err := filepath.EvalSymlinks(o.Cwd); err != nil || real != cwd {
			continue
		}
		if d.host.OpenFiles != nil && slices.Contains(d.host.OpenFiles(o.PID), db) {
			return true
		}
	}
	return false
}

// modelsFile is $OPENCODE_MODELS_PATH, else <cache>/opencode/models.json with
// the cache being $XDG_CACHE_HOME or ~/.cache; relative paths are against the
// holder's cwd.
func (d *detector) modelsFile(holder agentctx.Proc) string {
	var env map[string]string
	if d.host.Env != nil {
		env = d.host.Env(holder.PID)
	}
	path := env["OPENCODE_MODELS_PATH"]
	if path == "" {
		cache := env["XDG_CACHE_HOME"]
		if cache == "" {
			home := env["HOME"]
			if home == "" {
				home = d.host.Home
			}
			if home == "" {
				return ""
			}
			cache = filepath.Join(home, ".cache")
		}
		path = filepath.Join(cache, name, "models.json")
	}
	if !filepath.IsAbs(path) {
		if holder.Cwd == "" {
			return ""
		}
		path = filepath.Join(holder.Cwd, path)
	}
	return path
}

// isOpencode reports whether the binary (or the script run by node) is opencode.
func isOpencode(argv []string) bool {
	var script string
	if len(argv) > 0 && filepath.Base(argv[0]) == "node" {
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				script = a
				break
			}
		}
	}
	for _, a := range append(slices.Clone(argv[:min(2, len(argv))]), script) {
		if a != "" && filepath.Base(a) == name {
			return true
		}
	}
	return false
}

// isDatabase matches the file named by OPENCODE_DB, else opencode*.db (not its
// -wal or -shm companions).
func isDatabase(path, named string) bool {
	base := filepath.Base(path)
	if named != "" {
		return base == named
	}
	return strings.HasPrefix(base, name) && filepath.Ext(base) == ".db"
}

func (d *detector) read(since time.Time) (agentctx.Reading, bool) {
	db, err := openDB(d.db)
	if err != nil {
		return agentctx.Reading{}, false
	}
	defer db.Close()
	id, ok := d.session(db, since)
	if !ok {
		return agentctx.Reading{}, false
	}
	msgs, ok := loadMessages(db, id)
	if !ok {
		return agentctx.Reading{}, false
	}
	return d.reading(msgs)
}

// openDB opens the database read-only. The caller closes it: nothing is held
// between ticks.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(100)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// session is the newest root, non-archived session updated since the agent
// started whose directory is the holder's cwd.
func (d *detector) session(db *sql.DB, since time.Time) (string, bool) {
	var ms int64
	if !since.IsZero() {
		ms = since.UnixMilli()
	}
	rows, err := db.Query(`SELECT id, directory FROM session WHERE parent_id IS NULL AND time_archived IS NULL `+
		`AND time_updated >= ?1 ORDER BY time_updated DESC`, ms)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	for rows.Next() {
		var id, dir string
		if rows.Scan(&id, &dir) != nil {
			return "", false
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil && real == d.cwd {
			return id, true
		}
	}
	return "", false
}

// loadMessages returns the newest messages of the session, newest first.
func loadMessages(db *sql.DB, id string) ([]message, bool) {
	rows, err := db.Query(`SELECT data FROM message WHERE session_id = ?1 `+
		`ORDER BY time_created DESC, id DESC LIMIT ?2`, id, messages)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var data string
		if rows.Scan(&data) != nil {
			return nil, false
		}
		var m message
		if json.Unmarshal([]byte(data), &m) == nil {
			out = append(out, m)
		}
	}
	return out, rows.Err() == nil
}

type message struct {
	Role     string `json:"role"`
	Provider string `json:"providerID"`
	Model    string `json:"modelID"`
	Picked   *struct {
		Provider string `json:"providerID"`
		Model    string `json:"modelID"`
	} `json:"model"`
	Tokens *struct {
		Input     uint64 `json:"input"`
		Output    uint64 `json:"output"`
		Reasoning uint64 `json:"reasoning"`
		Cache     struct {
			Read  uint64 `json:"read"`
			Write uint64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// key is the provider and model a message ran on: assistant messages carry
// them on the message, user messages in the model they picked.
func (m message) key() (provider, model string, ok bool) {
	if m.Role == "assistant" {
		provider, model = m.Provider, m.Model
	} else if m.Picked != nil {
		provider, model = m.Picked.Provider, m.Picked.Model
	}
	if provider == "" || strings.TrimSpace(model) == "" {
		return "", "", false
	}
	return provider, model, true
}

// used is the context a finished-or-streaming reply occupies; zero until it
// has produced output.
func (m message) used() uint64 {
	t := m.Tokens
	if t == nil || t.Output == 0 {
		return 0
	}
	var sum uint64
	for _, n := range []uint64{t.Input, t.Output, t.Reasoning, t.Cache.Read, t.Cache.Write} {
		if sum+n < sum {
			return ^uint64(0)
		}
		sum += n
	}
	return sum
}

func (d *detector) reading(newestFirst []message) (agentctx.Reading, bool) {
	var provider, model string
	found := false
	for _, m := range newestFirst {
		if provider, model, found = m.key(); found {
			break
		}
	}
	if !found {
		return agentctx.Reading{}, false
	}
	info, known := d.models.find(d.modelsPath, provider, model)
	r := agentctx.Reading{Model: model}
	if known && info.name != "" {
		r.Model = info.name
	}
	for _, m := range newestFirst {
		if m.Role != "assistant" || m.used() == 0 {
			continue
		}
		if p, mod, _ := m.key(); known && info.window > 0 && p == provider && mod == model {
			r.Percent = int(min((m.used()*100+info.window/2)/info.window, 100))
			r.HasPercent = true
		}
		break
	}
	return r, true
}
