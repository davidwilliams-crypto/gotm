//go:build windows

package metrics

import (
	"errors"
	"reflect"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Exercise both public readers at the Windows enumeration boundary. Opening the
// SCM is read-only; synthetic names never open or alter a real service.
func TestServiceEnumerationPages(t *testing.T) {
	readers := []struct {
		name string
		kind uint32
		read func() ([]string, error)
	}{
		{"services", serviceWin32, func() ([]string, error) {
			rows, err := ListServices()
			var names []string
			for _, row := range rows {
				names = append(names, row.Name)
			}
			return names, err
		}},
		{"drivers", windows.SERVICE_DRIVER, func() ([]string, error) {
			rows, err := ReadDrivers()
			var names []string
			for _, row := range rows {
				names = append(names, row.Name)
			}
			return names, err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				pages   [][]string
				lastErr error
			}{
				{"empty", [][]string{nil}, nil},
				{"one page", [][]string{{"gotm-test-enum-a"}}, nil},
				{"multiple pages", [][]string{{"gotm-test-enum-a"}, {"gotm-test-enum-b"}, {"gotm-test-enum-c"}}, nil},
				{"later error", [][]string{{"gotm-test-enum-a"}, nil}, windows.ERROR_ACCESS_DENIED},
			} {
				t.Run(tc.name, func(t *testing.T) {
					old := enumServicesStatusEx
					t.Cleanup(func() { enumServicesStatusEx = old })
					call := 0
					enumServicesStatusEx = func(_ windows.Handle, level, kind, state uint32, buf *byte, size uint32, needed, returned, resume *uint32, _ *uint16) error {
						if level != scEnumProcessInfo || kind != reader.kind || state != serviceStateAll {
							t.Fatal("incorrect enumeration filter")
						}
						if size > 256*1024 {
							t.Fatalf("oversized buffer: %d", size)
						}
						if buf == nil {
							if tc.name == "empty" {
								*returned = 0
								return nil
							}
							*needed = 4096
							*returned = 0
							return windows.ERROR_MORE_DATA
						}
						if call >= len(tc.pages) {
							t.Fatal("enumeration did not terminate")
						}
						if *resume != uint32(call) {
							t.Fatalf("resume = %d, want %d", *resume, call)
						}
						page := tc.pages[call]
						// Match the Windows page lifetime: the next call overwrites
						// both the records and the strings inside the same buffer.
						raw := unsafe.Slice(buf, size)
						clear(raw)
						entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(buf)), len(page))
						offset := len(page) * int(unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{}))
						for i, name := range page {
							text, _ := windows.UTF16FromString(name)
							p := (*uint16)(unsafe.Pointer(&raw[offset]))
							copy(unsafe.Slice(p, len(text)), text)
							offset += len(text) * 2
							entries[i] = windows.ENUM_SERVICE_STATUS_PROCESS{ServiceName: p, DisplayName: p}
						}
						*returned = uint32(len(page))
						call++
						if call == len(tc.pages) {
							*resume = 0
							return tc.lastErr
						}
						*needed = 512 * 1024
						*resume = uint32(call)
						return windows.ERROR_MORE_DATA
					}
					names, err := reader.read()
					if !errors.Is(err, tc.lastErr) {
						t.Fatalf("error = %v, want %v", err, tc.lastErr)
					}
					if tc.lastErr != nil {
						if len(names) != 0 {
							t.Fatalf("incomplete enumeration returned rows: %v", names)
						}
						return
					}
					var want []string
					for _, page := range tc.pages {
						want = append(want, page...)
					}
					if !reflect.DeepEqual(names, want) {
						t.Fatalf("rows = %v, want %v", names, want)
					}
				})
			}
		})
	}
}

func TestServiceEnumerationStopsWithoutProgress(t *testing.T) {
	for _, rows := range []uint32{0, 1} {
		t.Run(map[uint32]string{0: "empty pages", 1: "repeated resume"}[rows], func(t *testing.T) {
			old := enumServicesStatusEx
			t.Cleanup(func() { enumServicesStatusEx = old })
			calls := 0
			enumServicesStatusEx = func(_ windows.Handle, _, _, _ uint32, buf *byte, size uint32, needed, returned, resume *uint32, _ *uint16) error {
				calls++
				if calls > 5 {
					t.Fatal("enumeration kept retrying without progress")
				}
				if size > 256*1024 {
					t.Fatalf("oversized buffer: %d", size)
				}
				*needed, *returned = 512*1024, 0
				if rows != 0 && buf != nil {
					*returned, *resume = 1, 7
					*(*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(buf)) = windows.ENUM_SERVICE_STATUS_PROCESS{}
				}
				return windows.ERROR_MORE_DATA
			}
			if err := enumerateServiceStatus(0, serviceWin32, func([]windows.ENUM_SERVICE_STATUS_PROCESS) {}); err == nil {
				t.Fatal("expected an error for enumeration without progress")
			}
		})
	}
}
