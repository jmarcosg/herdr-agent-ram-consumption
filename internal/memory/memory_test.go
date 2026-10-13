package memory

import (
	"os"
	"os/exec"
	"testing"

	"github.com/jmarcosg/herdr-agent-ram-consumption/internal/process"
)

func TestMeasure(t *testing.T) {
	self, _ := process.Footprint(os.Getpid())
	got, ok := Measure(os.Getpid())
	if !ok || got < self || got == 0 {
		t.Fatalf("Measure(self) = %d, %v; want >= %d", got, ok, self)
	}
}

func TestMeasureDead(t *testing.T) {
	cmd := exec.Command("sleep", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got, ok := Measure(pid); ok {
		t.Fatalf("Measure(dead) = %d, true; want false", got)
	}
}
