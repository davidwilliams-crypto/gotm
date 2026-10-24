//go:build windows

package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// UserWritable reports whether this (unelevated) user can create files in dir,
// i.e. whether a program running as the user could replace gotm.exe there.
// It probes with a uniquely named file that is removed at once.
func UserWritable(dir string) bool {
	if IsElevated() {
		return false // an elevated probe proves nothing about ordinary users
	}
	p := filepath.Join(dir, ".gotm-write-probe-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	_ = os.Remove(p)
	return true
}
