// Package herdr wraps the herdr CLI: listing agents, reading a pane's process
// tree and publishing sidebar tokens.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ErrPaneGone means herdr no longer knows the pane.
var ErrPaneGone = errors.New("pane gone")

// Runner executes the herdr binary with argv (never a shell). On a non-zero
// exit it returns the captured stdout (may hold a JSON error body) and an
// error carrying the first non-empty stderr line.
type Runner func(ctx context.Context, bin string, args ...string) (stdout []byte, err error)

// Agent is one agent pane as listed by herdr.
type Agent struct{ PaneID, Kind, SessionID, Cwd string }

// Proc is a foreground process of a pane.
type Proc struct {
	PID   int
	Argv0 string
	Argv  []string
	Cwd   string
}

// ProcessInfo is a pane's shell and foreground process group.
type ProcessInfo struct {
	ShellPID, ForegroundPGID int
	Procs                    []Proc
}

// CLI shells out to herdr.
type CLI struct {
	bin string
	run Runner
}

// New returns a CLI that invokes bin through run.
func New(bin string, run Runner) *CLI { return &CLI{bin: bin, run: run} }

// BinPath returns HERDR_BIN_PATH or "herdr".
func BinPath(getenv func(string) string) string {
	if p := getenv("HERDR_BIN_PATH"); p != "" {
		return p
	}
	return "herdr"
}

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Error  *apiError `json:"error"`
	Result struct {
		Agents []struct {
			PaneID       string `json:"pane_id"`
			Agent        string `json:"agent"`
			AgentSession struct {
				Value string `json:"value"`
			} `json:"agent_session"`
			Cwd string `json:"cwd"`
		} `json:"agents"`
		ProcessInfo struct {
			ShellPID           int `json:"shell_pid"`
			ForegroundPGID     int `json:"foreground_process_group_id"`
			ForegroundProcList []struct {
				PID   int      `json:"pid"`
				Argv0 string   `json:"argv0"`
				Argv  []string `json:"argv"`
				Cwd   string   `json:"cwd"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	} `json:"result"`
}

// call runs herdr and decodes the reply. A JSON error body becomes a Go error
// whether herdr exited 0 or not (a failing exit still prints it on stdout); a
// bare non-zero exit carries stderr.
func (c *CLI) call(ctx context.Context, args ...string) (*envelope, error) {
	out, runErr := c.run(ctx, c.bin, args...)
	var env envelope
	// Some commands print nothing on success.
	if runErr == nil && len(bytes.TrimSpace(out)) == 0 {
		return &env, nil
	}
	jsonErr := json.Unmarshal(bytes.TrimSpace(out), &env)
	if jsonErr == nil && env.Error != nil {
		return nil, fromAPI(env.Error)
	}
	cmd := strings.Join(args[:min(2, len(args))], " ")
	if runErr != nil {
		return nil, fmt.Errorf("herdr %s: %w", cmd, runErr)
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("herdr %s: decode reply: %w", cmd, jsonErr)
	}
	return &env, nil
}

func fromAPI(e *apiError) error {
	err := fmt.Errorf("herdr error %s: %s", e.Code, e.Message)
	if e.Code == "pane_not_found" {
		return fmt.Errorf("%w: %w", ErrPaneGone, err)
	}
	return err
}

// Agents lists every agent pane.
func (c *CLI) Agents(ctx context.Context) ([]Agent, error) {
	env, err := c.call(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(env.Result.Agents))
	for _, a := range env.Result.Agents {
		out = append(out, Agent{PaneID: a.PaneID, Kind: a.Agent, SessionID: a.AgentSession.Value, Cwd: a.Cwd})
	}
	return out, nil
}

// ProcessInfo returns the pane's foreground processes, or ErrPaneGone.
func (c *CLI) ProcessInfo(ctx context.Context, paneID string) (ProcessInfo, error) {
	env, err := c.call(ctx, "pane", "process-info", "--pane", paneID)
	if err != nil {
		return ProcessInfo{}, err
	}
	pi := env.Result.ProcessInfo
	info := ProcessInfo{ShellPID: pi.ShellPID, ForegroundPGID: pi.ForegroundPGID}
	for _, p := range pi.ForegroundProcList {
		info.Procs = append(info.Procs, Proc{PID: p.PID, Argv0: p.Argv0, Argv: p.Argv, Cwd: p.Cwd})
	}
	return info, nil
}

// ReportTokens sets and clears this plugin's sidebar tokens on a pane. Set
// tokens go first sorted by name, then clears sorted, so argv is stable. A pane
// that vanished is not an error.
func (c *CLI) ReportTokens(ctx context.Context, paneID string, set map[string]string, clear []string) error {
	args := []string{"pane", "report-metadata", paneID, "--source", "agent-stats"}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		args = append(args, "--token", k+"="+set[k])
	}
	clear = append([]string(nil), clear...)
	sort.Strings(clear)
	for _, k := range clear {
		args = append(args, "--clear-token", k)
	}
	if _, err := c.call(ctx, args...); err != nil && !errors.Is(err, ErrPaneGone) {
		return err
	}
	return nil
}
