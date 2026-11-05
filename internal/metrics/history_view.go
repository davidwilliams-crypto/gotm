//go:build windows

package metrics

import "sort"

// HistoryInfo describes the sampler's history ring at the moment of a read.
type HistoryInfo struct {
	Len            int   // points held
	Oldest, Newest int64 // first and last T (unix ms); zero when empty
	// Prev is the T of the newest point not newer than the requested time
	// (zero when there is none). A consumer whose newest point is no longer
	// in the ring (a clock rollback replaced it) sees Prev differ from it.
	Prev int64
}

// AppendHistoryAfter appends to dst shallow copies of the history points whose
// T is newer than after (unix ms), oldest first, and reports the ring's
// bounds at the same instant. It lets a consumer mirror the history
// incrementally instead of copying the whole ring every tick: append the
// returned points, drop its own points older than info.Oldest, and fall back
// to History() when the result does not reconcile (info.Newest older than the
// consumer's newest point after a clock rollback, or a length mismatch).
//
// Ownership is the same as History(): HistPoint values are copied, while their
// Cores and GPUs (and each GPUHist.Eng map) are shared with the ring. The
// sampler builds those for every tick and never mutates them afterwards; the
// eviction in tick only clears ring slots, never the arrays they point to.
// Consumers must treat them as read-only.
func (s *Sampler) AppendHistoryAfter(dst []HistPoint, after int64) ([]HistPoint, HistoryInfo) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := HistoryInfo{Len: len(s.hist)}
	if n := len(s.hist); n > 0 {
		info.Oldest, info.Newest = s.hist[0].T, s.hist[n-1].T
	}
	// The ring is chronological: tick drops any point newer than the sample
	// it appends after a clock rollback.
	i := sort.Search(len(s.hist), func(i int) bool { return s.hist[i].T > after })
	if i > 0 {
		info.Prev = s.hist[i-1].T
	}
	return append(dst, s.hist[i:]...), info
}
