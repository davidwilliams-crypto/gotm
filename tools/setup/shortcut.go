//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createShortcut writes a .lnk through the shell's IShellLinkW/IPersistFile
// COM interfaces (vtable calls; no cgo, no scripting host).
func createShortcut(lnk, target, workDir, desc string) error {
	// COM apartments belong to OS threads, and a goroutine may wake up on a
	// different thread after any system call: pin it, or CoCreateInstance can
	// fail with CO_E_NOTINITIALIZED on a thread that never saw CoInitializeEx.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	switch err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err {
	case nil, syscall.Errno(1): // S_OK, S_FALSE (already initialised on this thread)
		defer windows.CoUninitialize()
	case syscall.Errno(0x80010106): // RPC_E_CHANGED_MODE: COM is up in another mode; usable
	default:
		return fmt.Errorf("CoInitializeEx: %w", err)
	}

	var link uintptr
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if int32(hr) < 0 || link == 0 {
		return fmt.Errorf("CoCreateInstance(ShellLink): 0x%08x", uint32(hr))
	}
	defer comCall(link, 2) // Release

	// UTF-16 copies are held in ptrs until the calls finish; a bare uintptr
	// would not keep them alive.
	var ptrs []*uint16
	str := func(s string) uintptr {
		p, _ := windows.UTF16PtrFromString(s)
		ptrs = append(ptrs, p)
		return uintptr(unsafe.Pointer(p))
	}
	defer runtime.KeepAlive(&ptrs)
	for _, c := range []struct {
		slot int
		args []uintptr
	}{
		{20, []uintptr{str(target)}},    // SetPath
		{9, []uintptr{str(workDir)}},    // SetWorkingDirectory
		{7, []uintptr{str(desc)}},       // SetDescription
		{17, []uintptr{str(target), 0}}, // SetIconLocation
	} {
		if hr := comCall(link, c.slot, c.args...); int32(hr) < 0 {
			return fmt.Errorf("IShellLinkW slot %d: 0x%08x", c.slot, uint32(hr))
		}
	}

	var pf uintptr
	if hr := comCall(link, 0, uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&pf))); int32(hr) < 0 || pf == 0 {
		return fmt.Errorf("QueryInterface(IPersistFile): 0x%08x", uint32(hr))
	}
	defer comCall(pf, 2)
	if hr := comCall(pf, 6, str(lnk), 1); int32(hr) < 0 { // IPersistFile::Save
		return fmt.Errorf("save %s: 0x%08x", lnk, uint32(hr))
	}
	return nil
}

const clsctxInprocServer = 1

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	clsidShellLink = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW  = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile = windows.GUID{Data1: 0x0000010B, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// comCall invokes vtable slot n of COM object obj (this in the first argument).
//
//go:uintptrescapes
func comCall(obj uintptr, slot int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(ptrOf(obj))
	fn := *(*uintptr)(ptrOf(vtbl + uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	return r
}

// ptrOf turns an address owned by COM (not Go) back into a pointer.
func ptrOf(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }
