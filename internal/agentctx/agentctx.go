// Package agentctx defines the contract shared by the per-agent detectors that
// report a pane's model and context usage.
package agentctx

import "time"

// Pane describes one agent pane as seen by herdr.
type Pane struct {
	ID, Kind, SessionID, Cwd string
	AgentPID                 int
	Argv                     []string  // agent process argv
	Start                    time.Time // agent process start
}

// Reading is what a detector learned about a pane.
type Reading struct {
	Model      string // display-ready
	Percent    int
	HasPercent bool
}

// Detector reads a pane's model and context usage. The bool is false when the
// pane's state is unknown.
type Detector interface{ Read(p Pane) (Reading, bool) }

// Proc is a descendant process of an agent.
type Proc struct {
	PID  int
	Argv []string
	Cwd  string
}

// Host is everything a detector may ask of the machine. Detectors go through it
// instead of calling the process package so tests can fake it.
type Host struct {
	Home        string                          // $HOME
	Getenv      func(string) string             // this process's env
	Env         func(pid int) map[string]string // env of another process
	OpenFiles   func(pid int) []string
	Descendants func(pid int) []Proc
	Processes   func() []Proc // every process visible to the user
}
