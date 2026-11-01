//go:build windows

package ui

import (
	"testing"

	"gotm/internal/metrics"
)

func TestHoverIndexAtMapsMouseToSample(t *testing.T) {
	p := &perfPage{hoverIdx: -1, hoverGraphOn: true,
		hoverGraph: rect{X: 10, Y: 10, W: 100, H: 50},
		hist:       make([]metrics.HistPoint, 5)}
	for i := range p.hist {
		p.hist[i].T = int64(i)
	}
	if got := p.hoverIndexAt(10, 20); got != 0 {
		t.Fatalf("left edge sample=%d, want 0", got)
	}
	if got := p.hoverIndexAt(60, 35); got != 2 { // 50% across the four steps
		t.Fatalf("centre sample=%d, want 2", got)
	}
	if got := p.hoverIndexAt(109, 35); got != 4 {
		t.Fatalf("right edge sample=%d, want 4", got)
	}
	if got := p.hoverIndexAt(200, 35); got != -1 {
		t.Fatalf("off-graph sample=%d, want -1", got)
	}
	p.hoverGraphOn = false
	if got := p.hoverIndexAt(60, 35); got != -1 {
		t.Fatal("hover mapped a sample while the graph is not shown")
	}
}
