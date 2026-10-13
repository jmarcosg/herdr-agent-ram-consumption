// Package jsonl reads appended complete lines from growing JSONL files.
package jsonl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// Window is the most bytes read per call, and the most a first read looks back.
const Window = 1 << 20

// Tail incrementally reads complete lines from one file. The zero value is ready.
type Tail struct {
	seen     bool
	inode    uint64
	size     int64
	mtime    time.Time
	offset   int64
	skipping bool
}

// Read returns the complete lines appended since the previous call, without
// their newlines. reset is true on the first read and whenever internal state
// was discarded (rotation, truncation, rewrite); callers must then drop
// everything derived from earlier reads. The returned slices are freshly
// allocated and safe to retain.
func (t *Tail) Read(path string) (lines [][]byte, reset bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			*t = Tail{}
		}
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, false, fmt.Errorf("stat %s: unsupported platform", path)
	}
	inode, size, mtime := uint64(st.Ino), fi.Size(), fi.ModTime()

	reset = !t.seen ||
		t.inode != inode || size < t.size || (size == t.size && !mtime.Equal(t.mtime))
	if reset {
		*t = Tail{}
	} else if size == t.size && t.offset == size {
		return nil, false, nil
	}

	start := t.offset
	if start == 0 {
		start = max(size-Window, 0)
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, start, min(size-start, Window)))
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}

	if end := bytes.LastIndexByte(buf, '\n') + 1; end > 0 {
		from := 0
		if t.skipping || (t.offset == 0 && start > 0) {
			// Mid-line start: drop the partial first line.
			from = end
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				from = i + 1
			}
		}
		lines = splitLines(buf[from:end])
		t.offset = start + int64(end)
		t.skipping = false
	} else if len(buf) == Window {
		// A whole window with no newline: oversized line, discard through its end.
		t.offset = start + Window
		t.skipping = true
	}
	t.seen, t.inode, t.size, t.mtime = true, inode, size, mtime
	return lines, reset, nil
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		lines = append(lines, b[:i:i])
		b = b[i+1:]
	}
	return lines
}
