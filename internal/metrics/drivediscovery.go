//go:build windows

package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// Query the DOS device namespace instead of guessing a contiguous range of
// drive numbers. Servers and hot-plug systems can have sparse, large indices.
func discoverPhysicalDrives() ([]int, error) {
	for size := 4096; size <= 1<<24; size *= 2 {
		buffer := make([]uint16, size)
		n, err := windows.QueryDosDevice(nil, &buffer[0], uint32(len(buffer)))
		if err == windows.ERROR_INSUFFICIENT_BUFFER {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("enumerate physical drives: %w", err)
		}
		return physicalDriveIndices(buffer[:n]), nil
	}
	return nil, fmt.Errorf("enumerate physical drives: %w", windows.ERROR_INSUFFICIENT_BUFFER)
}

func physicalDriveIndices(names []uint16) []int {
	seen := make(map[int]bool)
	var indices []int
	for start := 0; start < len(names) && names[start] != 0; {
		end := start
		for end < len(names) && names[end] != 0 {
			end++
		}
		name := windows.UTF16ToString(names[start:end])
		start = end + 1
		if !strings.HasPrefix(name, "PhysicalDrive") {
			continue
		}
		suffix := strings.TrimPrefix(name, "PhysicalDrive")
		if suffix == "" || strings.IndexFunc(suffix, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			continue
		}
		index, err := strconv.Atoi(suffix)
		if err == nil && !seen[index] {
			seen[index] = true
			indices = append(indices, index)
		}
	}
	sort.Ints(indices)
	return indices
}
