//go:build windows

package metrics

import "unsafe"

// performanceInformation mirrors PERFORMANCE_INFORMATION (psapi.h).
type performanceInformation struct {
	Cb             uint32
	CommitTotal    uintptr
	CommitLimit    uintptr
	CommitPeak     uintptr
	PhysicalTotal  uintptr
	PhysicalAvail  uintptr
	SystemCache    uintptr
	KernelTotal    uintptr
	KernelPaged    uintptr
	KernelNonpaged uintptr
	PageSize       uintptr
	HandleCount    uint32
	ProcessCount   uint32
	ThreadCount    uint32
}

// MemInfo is a system-wide memory summary, in bytes unless noted.
type MemInfo struct {
	Total          uint64  `json:"total"`
	Available      uint64  `json:"available"`
	Used           uint64  `json:"used"`
	Percent        float64 `json:"percent"`
	Cached         uint64  `json:"cached"`
	CommitTotal    uint64  `json:"commitTotal"`
	CommitLimit    uint64  `json:"commitLimit"`
	KernelPaged    uint64  `json:"kernelPaged"`
	KernelNonpaged uint64  `json:"kernelNonpaged"`
	Handles        uint32  `json:"handles"`
	Processes      uint32  `json:"processes"`
	Threads        uint32  `json:"threads"`
}

func readMemory() (MemInfo, bool) {
	var pi performanceInformation
	pi.Cb = uint32(unsafe.Sizeof(pi))
	r, _, _ := procGetPerformanceInfo.Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.Cb))
	if r == 0 {
		return MemInfo{}, false
	}
	ps := uint64(pi.PageSize)
	m := MemInfo{
		Total:          uint64(pi.PhysicalTotal) * ps,
		Available:      uint64(pi.PhysicalAvail) * ps,
		Cached:         uint64(pi.SystemCache) * ps,
		CommitTotal:    uint64(pi.CommitTotal) * ps,
		CommitLimit:    uint64(pi.CommitLimit) * ps,
		KernelPaged:    uint64(pi.KernelPaged) * ps,
		KernelNonpaged: uint64(pi.KernelNonpaged) * ps,
		Handles:        pi.HandleCount,
		Processes:      pi.ProcessCount,
		Threads:        pi.ThreadCount,
	}
	if m.Total > m.Available {
		m.Used = m.Total - m.Available
	}
	if m.Total > 0 {
		m.Percent = float64(m.Used) * 100 / float64(m.Total)
	}
	return m, true
}
