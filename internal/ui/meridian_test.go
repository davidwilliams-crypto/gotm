//go:build windows

package ui

import (
	"testing"

	"github.com/tailscale/walk"
)

func contrast(a, b walk.Color) float64 {
	la, lb := luminance(a), luminance(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// Meridian asks for 4.5:1 for text on the grounds its tokens name, and 3:1 for
// control outlines and focus rings. The resource channel colours are gotm's
// own extension and carry small numeric labels, so they are held to 4.5:1 too.
func TestMeridianDarkContrast(t *testing.T) {
	p := meridianDark()
	for _, c := range []struct {
		name   string
		fg, bg walk.Color
		min    float64
	}{
		{"ink on canvas", p.Text, p.Bg, 4.5},
		{"ink on surface", p.Text, p.Card, 4.5},
		{"ink on surface-sunken", p.Text, p.Sunken, 4.5},
		{"ink on surface-tint", p.Text, p.Tint, 4.5},
		{"ink on hover", p.Text, p.CardHi, 4.5},
		{"ink-muted on canvas", p.Muted, p.Bg, 4.5},
		{"ink-muted on surface", p.Muted, p.Card, 4.5},
		{"ink-muted on surface-sunken", p.Muted, p.Sunken, 4.5},
		{"on-accent on accent", p.OnAccent, p.Accent, 4.5},
		{"on-accent on accent-hover", p.OnAccent, p.AccentHover, 4.5},
		{"accent-text on canvas", p.AccentInk, p.Bg, 4.5},
		{"accent-text on surface", p.AccentInk, p.Card, 4.5},
		{"accent-text on surface-tint", p.AccentInk, p.Tint, 4.5},
		{"success on surface", p.Good, p.Card, 4.5},
		{"success on success-tint", p.Good, p.GoodTint, 4.5},
		{"warning on surface", p.Warn, p.Card, 4.5},
		{"warning on warning-tint", p.Warn, p.WarnTint, 4.5},
		{"danger on surface", p.Bad, p.Card, 4.5},
		{"danger on danger-tint", p.Bad, p.BadTint, 4.5},
		{"selected row text", p.RowSelText, p.RowSel, 4.5},
		{"border-strong on canvas", p.Stroke, p.Bg, 3},
		{"border-strong on surface", p.Stroke, p.Card, 3},
		{"focus on canvas", p.Focus, p.Bg, 3},
		{"focus on surface", p.Focus, p.Card, 3},
		{"CPU on surface", p.CPU, p.Card, 4.5},
		{"Mem on surface", p.Mem, p.Card, 4.5},
		{"GPU on surface", p.GPU, p.Card, 4.5},
		{"Disk on surface", p.Disk, p.Card, 4.5},
		{"Net on surface", p.Net, p.Card, 4.5},
		{"Power on surface", p.Power, p.Card, 4.5},
		{"Thermal on surface", p.Thermal, p.Card, 4.5},
		{"Clock on surface", p.Clock, p.Card, 4.5},
		{"CPU on surface-tint", p.CPU, p.Tint, 4.5},
	} {
		if got := contrast(c.fg, c.bg); got < c.min {
			t.Errorf("%s: contrast %.2f, want at least %.1f", c.name, got, c.min)
		}
	}
}

func TestCapRadius(t *testing.T) {
	if got := capRadius(rect{0, 0, 100, 24}, rFull); got != 12 {
		t.Errorf("pill radius = %d, want 12", got)
	}
	if got := capRadius(rect{0, 0, 100, 80}, rLg); got != rLg {
		t.Errorf("card radius = %d, want %d", got, rLg)
	}
	if got := capRadius(rect{0, 0, 3, 3}, rSm); got != 1 {
		t.Errorf("tiny radius = %d, want 1", got)
	}
}
