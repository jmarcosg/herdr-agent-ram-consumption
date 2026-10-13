package process

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"
)

// start runs a command in its own process group and guarantees it (and any
// grandchildren) are killed and reaped when the test ends.
func start(t *testing.T, dir string, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestFootprintSelf(t *testing.T) {
	got, ok := Footprint(os.Getpid())
	if !ok || got == 0 {
		t.Fatalf("Footprint(self) = %d, %v; want > 0, true", got, ok)
	}
}

func TestFootprintDead(t *testing.T) {
	if got, ok := Footprint(deadPid(t)); ok {
		t.Fatalf("Footprint(dead) = %d, true; want false", got)
	}
}

func TestChildrenAndDescendants(t *testing.T) {
	cmd := start(t, "", "sh", "-c", "sleep 5 & wait")
	self := os.Getpid()

	var got []int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got = Descendants(self, 256)
		if slices.Contains(got, cmd.Process.Pid) && len(got) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(got, cmd.Process.Pid) || len(got) < 2 {
		t.Fatalf("Descendants = %v; want sh pid %d plus its sleep", got, cmd.Process.Pid)
	}
	if slices.Contains(got, self) {
		t.Fatalf("Descendants must exclude the root pid, got %v", got)
	}
	if capped := Descendants(self, 1); len(capped) != 1 {
		t.Fatalf("Descendants(max=1) = %v; want length 1", capped)
	}
}

func TestStartTime(t *testing.T) {
	got, ok := StartTime(os.Getpid())
	if !ok {
		t.Fatal("StartTime(self) not ok")
	}
	if age := time.Since(got); age < 0 || age > time.Minute {
		t.Fatalf("StartTime(self) = %v (age %v); want within the last minute", got, age)
	}
}

// TestHelperSleep is not a real test: TestEnv re-executes the test binary to
// get a long-lived child whose environment is inspectable.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("AGENT_STATS_HELPER") == "1" {
		time.Sleep(10 * time.Second)
	}
}

func TestEnv(t *testing.T) {
	// macOS hides the environment of SIP-protected system binaries such as
	// /bin/sleep, so the child must be a non-system executable: this binary.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), "AGENT_STATS_HELPER=1", "FOO_AGENT_STATS=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	if got := Env(cmd.Process.Pid)["FOO_AGENT_STATS"]; got != "1" {
		t.Fatalf("Env[FOO_AGENT_STATS] = %q; want 1", got)
	}
}

func TestEnvUnreadable(t *testing.T) {
	if got := Env(deadPid(t)); got != nil {
		t.Fatalf("Env(dead) = %v; want nil", got)
	}
	if got := Argv(deadPid(t)); got != nil {
		t.Fatalf("Argv(dead) = %v; want nil", got)
	}
	if got := Cwd(deadPid(t)); got != "" {
		t.Fatalf("Cwd(dead) = %q; want empty", got)
	}
}

func TestArgvCwd(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := start(t, dir, "sleep", "5")
	pid := cmd.Process.Pid

	if got := Argv(pid); !reflect.DeepEqual(got, []string{"sleep", "5"}) {
		t.Fatalf("Argv = %v; want [sleep 5]", got)
	}
	got, err := filepath.EvalSymlinks(Cwd(pid))
	if err != nil || got != dir {
		t.Fatalf("Cwd = %q (%v); want %q", got, err, dir)
	}
}

func TestOpenFiles(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "open.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	want, err := filepath.EvalSymlinks(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	got := OpenFiles(os.Getpid())
	for i, p := range got {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			got[i] = r
		}
	}
	if !slices.Contains(got, want) {
		t.Fatalf("OpenFiles missing %q", want)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)
	for _, p := range got {
		if p == "/dev/null" || p == cwd {
			t.Fatalf("OpenFiles must only list regular files, found %q", p)
		}
	}
}

func procargsBuf(argc int32, rest string) []byte {
	return append(binary.NativeEndian.AppendUint32(nil, uint32(argc)), rest...)
}

func TestParseProcargs(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		argv []string
		env  map[string]string
	}{
		{"skips exec path and padding", procargsBuf(2, "/usr/bin/node\x00\x00\x00\x00node\x00gemini\x00"), []string{"node", "gemini"}, map[string]string{}},
		{"stops before environment", procargsBuf(1, "/bin/zsh\x00\x00-zsh\x00HOME=/Users/a\x00PATH=/bin\x00"), []string{"-zsh"}, map[string]string{"HOME": "/Users/a", "PATH": "/bin"}},
		{"env follows args and stops at empty entry", procargsBuf(1, "/bin/zsh\x00\x00-zsh\x00HOME=/Users/a\x00PATH=/bin\x00\x00ptr_munge=\x00"), []string{"-zsh"}, map[string]string{"HOME": "/Users/a", "PATH": "/bin"}},
		{"args that look like vars are not env", procargsBuf(2, "/usr/bin/env\x00\x00env\x00A=1\x00B=2\x00"), []string{"env", "A=1"}, map[string]string{"B": "2"}},
		{"short buffer", []byte{1, 0}, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv, env := parseProcargs(c.buf)
			if !reflect.DeepEqual(argv, c.argv) {
				t.Errorf("argv = %v; want %v", argv, c.argv)
			}
			if !reflect.DeepEqual(env, c.env) {
				t.Errorf("env = %v; want %v", env, c.env)
			}
		})
	}
}

func TestStatusFootprint(t *testing.T) {
	cases := []struct {
		name string
		text string
		want uint64
		ok   bool
	}{
		{"anon plus swap", "RssAnon:\t     128 kB\nRssFile:\t    1664 kB\nVmSwap:\t      64 kB\n", 192 * 1024, true},
		{"no swap line", "Name:\tsleep\nRssAnon:\t     128 kB\n", 128 * 1024, true},
		{"no anon line", "Name:\tkworker\nState:\tI (idle)\n", 0, false},
		{"not a number", "RssAnon:\t     lots kB\n", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := statusFootprint(c.text)
			if got != c.want || ok != c.ok {
				t.Fatalf("statusFootprint = %d, %v; want %d, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestAll(t *testing.T) {
	cmd := start(t, "", "sleep", "5")
	got := All()
	for _, want := range []int{os.Getpid(), cmd.Process.Pid} {
		if !slices.Contains(got, want) {
			t.Fatalf("All() = %d pids, missing %d", len(got), want)
		}
	}
}
