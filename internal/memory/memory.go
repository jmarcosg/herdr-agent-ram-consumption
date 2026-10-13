// Package memory measures the memory footprint of a process tree.
package memory

import "github.com/jmarcosg/herdr-agent-ram-consumption/internal/process"

const maxDescendants = 256

// Measure sums the footprint of root and its descendants (saturating). It
// reports false only when no process in the tree could be measured.
func Measure(root int) (uint64, bool) {
	var total uint64
	var any bool
	for _, pid := range append([]int{root}, process.Descendants(root, maxDescendants)...) {
		bytes, ok := process.Footprint(pid)
		if !ok {
			continue
		}
		any = true
		if total+bytes < total {
			total = ^uint64(0)
		} else {
			total += bytes
		}
	}
	return total, any
}
