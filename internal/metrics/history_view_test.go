//go:build windows

package metrics

import (
	"testing"
	"time"
)

func TestAppendHistoryAfterReturnsOnlyNewerPoints(t *testing.T) {
	s := NewSampler(1000, 60)
	if out, info := s.AppendHistoryAfter(nil, 0); len(out) != 0 || info != (HistoryInfo{}) {
		t.Fatalf("empty ring: %v %+v", out, info)
	}
	for i := int64(1); i <= 5; i++ {
		s.hist = append(s.hist, HistPoint{T: i * 1000, CPU: float64(i), Cores: []float64{float64(i)}})
	}
	keep := []HistPoint{{T: -1}}
	out, info := s.AppendHistoryAfter(keep, 3000)
	if len(out) != 3 || out[0].T != -1 || out[1].T != 4000 || out[2].T != 5000 {
		t.Fatalf("appended %+v", out)
	}
	if info != (HistoryInfo{Len: 5, Oldest: 1000, Newest: 5000, Prev: 3000}) {
		t.Fatalf("info %+v", info)
	}
	if _, info = s.AppendHistoryAfter(nil, 3500); info.Prev != 3000 {
		t.Fatalf("a time between points: prev %d", info.Prev)
	}
	if out, info = s.AppendHistoryAfter(nil, 9000); len(out) != 0 || info.Prev != 5000 {
		t.Fatalf("nothing newer: %v %+v", out, info)
	}
	// Copies: changing the ring slot afterwards does not change the result.
	out, _ = s.AppendHistoryAfter(nil, 4000)
	s.hist[4] = HistPoint{}
	if out[0].T != 5000 || out[0].Cores[0] != 5 {
		t.Fatal("result aliases the ring slot")
	}
}

func TestRecorderViewsMatchFullCopies(t *testing.T) {
	const base = int64(1_700_000_000_000)
	r := NewRecorder(time.Hour, 2*time.Second, "")
	if _, ok := r.Step(base, 0); ok {
		t.Fatal("step in an empty recording")
	}
	if n, _, _ := r.WindowInfo(0, 0); n != 0 {
		t.Fatal("window of an empty recording")
	}
	r.SetEnabled(true)
	for _, sec := range []int64{0, 2, 7, 9, 18, 20} {
		r.Add(&Snapshot{T: base + sec*1000, CPU: float64(sec)})
	}
	for _, w := range [][2]int64{{0, 0}, {base + 2000, base + 18_000}, {base + 2500, base + 8000}, {base + 30_000, 0}, {base - 5000, base - 1}} {
		want := r.Samples(w[0], w[1])
		got := r.AppendSamples(nil, w[0], w[1])
		n, first, last := r.WindowInfo(w[0], w[1])
		if len(got) != len(want) || n != len(want) {
			t.Fatalf("window %v: %d/%d samples, want %d", w, len(got), n, len(want))
		}
		for i := range want {
			if got[i].T != want[i].T {
				t.Fatalf("window %v: sample %d differs", w, i)
			}
		}
		if n > 0 && (first != want[0].T || last != want[n-1].T) {
			t.Fatalf("window %v: bounds %d..%d", w, first, last)
		}
	}
	all := r.Samples(0, 0)
	for _, at := range []int64{base - 9000, base, base + 1000, base + 4500, base + 8000, base + 13_500, base + 19_000, base + 99_000} {
		near, _ := r.Nearest(at)
		i := 0
		for all[i].T != near.T {
			i++
		}
		for _, off := range []int{-7, -1, 0, 1, 7} {
			got, ok := r.Step(at, off)
			want := all[min(max(i+off, 0), len(all)-1)]
			if !ok || got.T != want.T {
				t.Fatalf("Step(%d, %d) = %d, want %d", at-base, off, got.T-base, want.T-base)
			}
		}
	}
	if newest, ok := r.Newest(); !ok || newest != base+20_000 {
		t.Fatalf("newest %d", newest)
	}
	r.AddMark("a", base)
	if r.MarkCount() != 1 {
		t.Fatal("mark count")
	}
}
