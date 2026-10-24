//go:build windows

package ui

import (
	"github.com/tailscale/walk"
	"gotm/internal/config"
	"gotm/internal/metrics"
	"testing"
)

func TestOtherSettingsPreserveCurrentProcessSort(t *testing.T) {
	s := config.OpenAt(t.TempDir()) // never the real profile
	a := &App{store: s, cfg: s.Get(), sampler: metrics.NewSampler(1000, 60)}
	a.cfg.SortKey, a.cfg.SortDesc = "pid", false
	if !a.saveConfig(func(c *config.Config) { c.RefreshMs = 2000 }) {
		t.Fatal("save failed")
	}
	if got := s.Get(); got.SortKey != "pid" || got.SortDesc {
		t.Fatalf("live sort was overwritten: %s descending=%v", got.SortKey, got.SortDesc)
	}
}

func TestGridTextSortUsesWholeCell(t *testing.T) {
	m := &gridModel{ready: true, rows: [][]string{{"10 Things"}, {"2 Apps"}, {"10 Apps"}}, sortCol: -1}
	if err := m.Sort(0, walk.SortAscending); err != nil {
		t.Fatal(err)
	}
	if m.rows[0][0] != "10 Apps" || m.rows[1][0] != "10 Things" || m.rows[2][0] != "2 Apps" {
		t.Fatalf("text column treated as numbers: %v", m.rows)
	}
}

func TestGridUpdateKeepsNumericOrder(t *testing.T) {
	m := &gridModel{keyCol: 0, sortCol: 1, sortDesc: true, numeric: map[int]bool{1: true}, rows: [][]string{{"A", "20 MB"}, {"B", "10 MB"}}}
	rows := [][]string{{"A", "5 MB"}, {"B", "30 MB"}}
	if m.update(rows) {
		t.Fatal("changed order must reset the model")
	}
	m.set(rows)
	if m.rows[0][0] != "B" {
		t.Fatalf("numeric sort lost: %v", m.rows)
	}
}

func TestGridCannotUpdateWithoutIdentity(t *testing.T) {
	m := &gridModel{keyCol: -1, sortCol: -1, rows: [][]string{{"old"}}}
	if m.update([][]string{{"new"}}) {
		t.Fatal("unidentified row silently reused")
	}
}

func TestNumericUnavailableRowsRemainStable(t *testing.T) {
	m := &gridModel{sortCol: 1, sortDesc: true, numeric: map[int]bool{1: true}, rows: [][]string{{"A", ""}, {"B", ""}, {"C", ""}}}
	m.resort()
	if m.rows[0][0] != "A" || m.rows[1][0] != "B" || m.rows[2][0] != "C" {
		t.Fatalf("equal missing values changed order: %v", m.rows)
	}
}

func TestPruneNestedEmptySensorGroups(t *testing.T) {
	rows := [][]string{
		{"▼ CPU", "", "", "", "cpu", "", "g"},
		{"    ▼ Clocks", "", "", "", "clocks", "", "g"},
		{"▼ Memory", "", "", "", "mem", "", "g"},
		{"    Usage", "40%", "", "", "usage"},
	}
	got := pruneEmptyGroups(rows)
	if len(got) != 2 || got[0][4] != "mem" {
		t.Fatalf("orphan groups remained: %v", got)
	}
}
