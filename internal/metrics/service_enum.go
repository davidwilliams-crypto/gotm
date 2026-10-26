//go:build windows

package metrics

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var enumServicesStatusEx = windows.EnumServicesStatusEx

// EnumServicesStatusEx reports bytes needed for all remaining rows, which may
// exceed its 256 KiB buffer limit. Consume each page before reusing the buffer;
// its string pointers are valid only until the next enumeration call.
func enumerateServiceStatus(scm windows.Handle, kind uint32, visit func([]windows.ENUM_SERVICE_STATUS_PROCESS)) error {
	const maxBuffer = 256 * 1024
	var buf []byte
	var resume uint32
	seen := map[uint32]bool{0: true}
	for {
		var p *byte
		if len(buf) != 0 {
			p = &buf[0]
		}
		var needed, returned uint32
		err := enumServicesStatusEx(scm, scEnumProcessInfo, kind, serviceStateAll,
			p, uint32(len(buf)), &needed, &returned, &resume, nil)
		if err != nil && err != windows.ERROR_MORE_DATA {
			return err
		}
		if returned != 0 {
			if uintptr(returned) > uintptr(len(buf))/unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{}) {
				return fmt.Errorf("invalid service enumeration page size")
			}
			visit(unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(p)), returned))
		}
		if err == nil {
			return nil
		}
		if returned != 0 {
			if seen[resume] {
				return fmt.Errorf("service enumeration did not advance")
			}
			seen[resume] = true
			continue
		}
		// An empty page may need a larger buffer, but must not spin forever
		// when the API keeps asking for more data without returning rows.
		size := max(int(needed), max(len(buf)*2, 64*1024))
		size = min(size, maxBuffer)
		if size <= len(buf) {
			return fmt.Errorf("service enumeration made no progress")
		}
		buf = make([]byte, size)
	}
}
