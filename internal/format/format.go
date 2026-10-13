// Package format renders memory sizes and severity levels for display.
package format

import "fmt"

const (
	MB = 1 << 20
	GB = 1 << 30
)

// Size renders a byte count as whole megabytes below 1 GB, and as gigabytes
// with one decimal place from 1 GB upward.
func Size(bytes uint64) string {
	mb := (bytes + MB/2) / MB
	if mb < 1024 {
		return fmt.Sprintf("%d MB", mb)
	}
	tenths := (bytes*10 + GB/2) / GB
	return fmt.Sprintf("%d.%d GB", tenths/10, tenths%10)
}

// Severity classifies a usage percentage as "ok", "warn" or "crit".
func Severity(percent int) string {
	switch {
	case percent >= 90:
		return "crit"
	case percent >= 75:
		return "warn"
	default:
		return "ok"
	}
}
