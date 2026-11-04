//go:build windows

package metrics

import (
	"reflect"
	"testing"
	"unicode/utf16"
)

func TestPhysicalDriveIndicesSparseAndMalformed(t *testing.T) {
	names := "PhysicalDrive4096\x00PhysicalDrive0\x00PhysicalDrive78\x00PhysicalDrive78\x00PhysicalDrive\x00PhysicalDrive-1\x00PhysicalDrive+2\x00PhysicalDrive3x\x00PhysicalDrive99999999999999999999999\x00C:\x00PhysicalDrive00078\x00\x00"
	got := physicalDriveIndices(utf16.Encode([]rune(names)))
	if want := []int{0, 78, 4096}; !reflect.DeepEqual(got, want) {
		t.Fatalf("drive indices = %v, want %v", got, want)
	}
}

func TestPhysicalDriveIndicesEmpty(t *testing.T) {
	if got := physicalDriveIndices([]uint16{0, 0}); len(got) != 0 {
		t.Fatalf("empty inventory = %v", got)
	}
}

func TestDiscoverPhysicalDrivesLive(t *testing.T) {
	indices, err := discoverPhysicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Windows physical drive inventory: %v", indices)
	for i, index := range indices {
		if index < 0 || (i > 0 && index <= indices[i-1]) {
			t.Fatalf("inventory is not sorted and unique: %v", indices)
		}
	}
}
