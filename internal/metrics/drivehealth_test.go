//go:build windows

package metrics

import "testing"

// Reads this PC's drives; any temperature reported must be a real one (the old
// code asked for the wrong property and read the wrong offset, giving none).
func TestDriveTemperaturesArePlausible(t *testing.T) {
	for _, d := range ReadDriveHealth() {
		t.Logf("drive %d: %s [%s] temp=%v %.0f°C smart=%v", d.Index, d.Model, d.Bus, d.HasTemp, d.TempC, d.HasSMART)
		if d.HasTemp && (d.TempC <= 0 || d.TempC >= 100) {
			t.Errorf("drive %d temperature %.1f °C is not plausible", d.Index, d.TempC)
		}
	}
}
