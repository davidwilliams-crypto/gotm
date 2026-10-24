//go:build windows

package ui

import "fmt"

func fmtBytes(n uint64) string {
	const unit = 1024.0
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	switch {
	case i == 0 || f >= 100:
		return fmt.Sprintf("%.0f %s", f, units[i])
	case f >= 10:
		return fmt.Sprintf("%.1f %s", f, units[i])
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func fmtRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	return fmtBytes(uint64(bps)) + "/s"
}

func fmtPct(p float64) string { return fmt.Sprintf("%.1f%%", p) }

func fmtDur(sec uint64) string {
	d := sec / 86400
	h := sec % 86400 / 3600
	m := sec % 3600 / 60
	s := sec % 60
	if d > 0 {
		return fmt.Sprintf("%dd %02d:%02d:%02d", d, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
