//go:build windows

package metrics

import (
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"

	"golang.org/x/sys/windows"
)

// dosDeviceChars remembers the buffer size the DOS device list last needed,
// so later inventories do not grow from 4096 characters again.
var dosDeviceChars atomic.Int64

// Query the DOS device namespace instead of guessing a contiguous range of
// drive numbers. Servers and hot-plug systems can have sparse, large indices.
func discoverPhysicalDrives() ([]int, error) {
	size := int(dosDeviceChars.Load())
	if size < 4096 {
		size = 4096
	}
	for ; size <= 1<<24; size *= 2 {
		buffer := make([]uint16, size)
		n, err := windows.QueryDosDevice(nil, &buffer[0], uint32(len(buffer)))
		if err == windows.ERROR_INSUFFICIENT_BUFFER {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("enumerate physical drives: %w", err)
		}
		dosDeviceChars.Store(int64(size))
		return physicalDriveIndices(buffer[:n]), nil
	}
	return nil, fmt.Errorf("enumerate physical drives: %w", windows.ERROR_INSUFFICIENT_BUFFER)
}

const physicalDrivePrefix = "PhysicalDrive"

// physicalDriveIndices picks "PhysicalDrive<digits>" out of a NUL-separated
// UTF-16 list. Names are compared in UTF-16 and only the digits of a match
// are converted: the list holds hundreds of unrelated devices.
func physicalDriveIndices(names []uint16) []int {
	var indices []int
	for start := 0; start < len(names) && names[start] != 0; {
		end := start
		for end < len(names) && names[end] != 0 {
			end++
		}
		name := names[start:end]
		start = end + 1
		if len(name) <= len(physicalDrivePrefix) {
			continue
		}
		match := true
		for i := 0; i < len(physicalDrivePrefix); i++ {
			if name[i] != uint16(physicalDrivePrefix[i]) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		suffix := name[len(physicalDrivePrefix):]
		var digits [24]byte
		if len(suffix) > len(digits) {
			continue // longer than any int: Atoi would fail anyway
		}
		ok := true
		for i, c := range suffix {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			digits[i] = byte(c)
		}
		if !ok {
			continue
		}
		index, err := strconv.Atoi(string(digits[:len(suffix)]))
		if err == nil {
			indices = append(indices, index)
		}
	}
	slices.Sort(indices)
	return slices.Compact(indices)
}
