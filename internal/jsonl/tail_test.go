package jsonl

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func strs(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}

func mustRead(t *testing.T, tail *Tail, path string) ([]string, bool) {
	t.Helper()
	lines, reset, err := tail.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return strs(lines), reset
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTailFirstReadOnlyComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, "a\nb\nc")
	var tail Tail

	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"a", "b"}) || !reset {
		t.Fatalf("first read = %v reset=%v", got, reset)
	}
	appendTo(t, path, "d\n")
	got, reset = mustRead(t, &tail, path)
	if !equal(got, []string{"cd"}) || reset {
		t.Fatalf("second read = %v reset=%v", got, reset)
	}
	got, reset = mustRead(t, &tail, path)
	if len(got) != 0 || reset {
		t.Fatalf("idle read = %v reset=%v", got, reset)
	}
}

func TestTailWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	line := strings.Repeat("x", 99) + "\n"
	write(t, path, strings.Repeat(line, 3*(1<<20)/100))
	var tail Tail

	lines, reset, err := tail.Read(path)
	if err != nil || !reset {
		t.Fatalf("err=%v reset=%v", err, reset)
	}
	if len(lines) == 0 || len(lines)*100 > Window {
		t.Fatalf("got %d lines, want 1..%d", len(lines), Window/100)
	}
	for i, l := range lines {
		if string(l) != line[:99] {
			t.Fatalf("line %d is not complete: len=%d", i, len(l))
		}
	}
}

func TestTailTruncateResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, "one\ntwo\n")
	var tail Tail
	mustRead(t, &tail, path)

	write(t, path, "x\n")
	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"x"}) || !reset {
		t.Fatalf("got %v reset=%v", got, reset)
	}
}

func TestTailRotateResets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	write(t, path, "one\n")
	var tail Tail
	mustRead(t, &tail, path)

	next := filepath.Join(dir, "next.jsonl")
	write(t, next, "two\nthree\n")
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"two", "three"}) || !reset {
		t.Fatalf("got %v reset=%v", got, reset)
	}
}

func TestTailSameSizeMtimeResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, "aaa\n")
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, base, base); err != nil {
		t.Fatal(err)
	}
	var tail Tail
	mustRead(t, &tail, path)

	write(t, path, "bbb\n")
	later := base.Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"bbb"}) || !reset {
		t.Fatalf("got %v reset=%v", got, reset)
	}
}

func TestTailMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, "one\n")
	var tail Tail
	mustRead(t, &tail, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tail.Read(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	write(t, path, "one\n")
	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"one"}) || !reset {
		t.Fatalf("after recreate got %v reset=%v", got, reset)
	}
}

func TestTailSkipsOversizedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	write(t, path, strings.Repeat("o", 2<<20))
	var tail Tail

	lines, _, err := tail.Read(path)
	if err != nil || len(lines) != 0 {
		t.Fatalf("oversized read: lines=%d err=%v", len(lines), err)
	}
	if !tail.Skipped() {
		t.Fatal("Skipped() = false after discarding an oversized line")
	}
	// Ends the oversized line; its remainder must be discarded.
	appendTo(t, path, "tail\n")
	lines, _, err = tail.Read(path)
	if err != nil || len(lines) != 0 {
		t.Fatalf("remainder read: lines=%d err=%v", len(lines), err)
	}
	if !tail.Skipped() {
		t.Fatal("Skipped() = false after discarding the oversized remainder")
	}
	appendTo(t, path, "x\n")
	got, reset := mustRead(t, &tail, path)
	if !equal(got, []string{"x"}) || reset {
		t.Fatalf("got %v reset=%v", got, reset)
	}
	if tail.Skipped() {
		t.Fatal("Skipped() stayed true after a clean read")
	}
	for _, l := range got {
		if bytes.Contains([]byte(l), []byte("oo")) {
			t.Fatal("oversized line leaked")
		}
	}
}
