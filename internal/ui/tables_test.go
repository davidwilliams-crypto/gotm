//go:build windows

package ui

import "testing"

// Each refresh enumerates list views. Go allows only ~2000 callbacks per
// process and aborts the program past that, so the enumeration must reuse one.
func TestChildEnumerationDoesNotExhaustCallbacks(t *testing.T) {
	for i := 0; i < 2500; i++ {
		_ = childrenOfClass(0, "SysListView32") // parent 0 = top-level windows; cheap
	}
}
