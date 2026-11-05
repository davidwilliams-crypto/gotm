//go:build windows

package metrics

import "sort"

// Read-only views of the recording that avoid copying the whole ring. All of
// them hold the recorder's read lock only for a binary search and the copy of
// what they return.

// rangeLocked returns the ring indices [i, j) with from <= T <= to (0 =
// unbounded), exactly as Samples selects them. r.mu must be held.
func (r *Recorder) rangeLocked(from, to int64) (int, int) {
	i := sort.Search(len(r.ring), func(i int) bool { return from == 0 || r.ring[i].T >= from })
	j := len(r.ring)
	if to > 0 {
		j = sort.Search(len(r.ring), func(i int) bool { return r.ring[i].T > to })
	}
	if i > j {
		i = j
	}
	return i, j
}

// WindowInfo reports how many samples Samples(from, to) would return and the
// times of the first and last of them, without copying anything.
func (r *Recorder) WindowInfo(from, to int64) (n int, first, last int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i, j := r.rangeLocked(from, to)
	if i == j {
		return 0, 0, 0
	}
	return j - i, r.ring[i].T, r.ring[j-1].T
}

// AppendSamples appends to dst the samples Samples(from, to) would return, so
// a caller can reuse one buffer. Top slices are shared and read-only.
func (r *Recorder) AppendSamples(dst []RecSample, from, to int64) []RecSample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i, j := r.rangeLocked(from, to)
	return append(dst, r.ring[i:j]...)
}

// Step returns the sample offset positions after (or, when negative, before)
// the sample Nearest(t) returns, clamped to the oldest and newest samples.
func (r *Recorder) Step(t int64, offset int) (RecSample, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := len(r.ring)
	if n == 0 {
		return RecSample{}, false
	}
	i := sort.Search(n, func(i int) bool { return r.ring[i].T >= t })
	switch {
	case i >= n:
		i = n - 1
	case i > 0 && t-r.ring[i-1].T <= r.ring[i].T-t:
		i-- // the same tie-break as Nearest
	}
	i = min(max(i+offset, 0), n-1)
	return r.ring[i], true
}

// Newest returns the time of the newest sample held.
func (r *Recorder) Newest() (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.ring) == 0 {
		return 0, false
	}
	return r.ring[len(r.ring)-1].T, true
}

// MarkCount reports how many bookmarks exist; marks are only ever appended
// or cleared together with the samples.
func (r *Recorder) MarkCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.marks)
}
