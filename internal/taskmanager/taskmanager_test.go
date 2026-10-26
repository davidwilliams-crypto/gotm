//go:build windows

package taskmanager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const testExe = `C:\Program Files\gotm\gotm.exe`
const testCommand = `"C:\Program Files\gotm\gotm.exe" --task-manager --`
const newExe = `D:\Programs\gotm\gotm.exe`
const newCommand = `"D:\Programs\gotm\gotm.exe" --task-manager --`

// Every registry mutation in this package's tests is below a unique HKCU key.
func scratchStore(t *testing.T) Store {
	t.Helper()
	path := fmt.Sprintf(`Software\gotm-taskmanager-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS|registry.WOW64_64KEY)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	t.Cleanup(func() { deleteScratch(t, path) })
	return Store{Root: registry.CURRENT_USER, Path: path + `\taskmgr.exe`}
}

func deleteScratch(t *testing.T, path string) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return
	}
	if err != nil {
		t.Error(err)
		return
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		t.Error(err)
		return
	}
	for _, name := range names {
		deleteScratch(t, path+`\`+name)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
		t.Error(err)
	}
}

func scratchKey(t *testing.T, s Store) registry.Key {
	t.Helper()
	k, _, err := registry.CreateKey(s.Root, s.Path, registry.ALL_ACCESS|registry.WOW64_64KEY)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

func mustSet(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireState(t *testing.T, s Store, exe string, want State) {
	t.Helper()
	got, err := s.Query(exe)
	if err != nil || got != want {
		t.Fatalf("Query(%q) = %+v, %v; want %+v", exe, got, err, want)
	}
}

func TestDebuggerCommandQuotesPathAndSeparatesAppendedArguments(t *testing.T) {
	for _, tc := range []struct{ exe, want string }{
		{testExe, testCommand},
		{`D:\Apps\é & tools\gotm.exe`, `"D:\Apps\é & tools\gotm.exe" --task-manager --`},
	} {
		got, err := DebuggerCommand(tc.exe)
		if err != nil || got != tc.want {
			t.Errorf("DebuggerCommand(%q) = %q, %v; want %q", tc.exe, got, err, tc.want)
		}
	}
}

func TestDebuggerCommandRejectsRelativeAndUnsafePaths(t *testing.T) {
	for _, exe := range []string{"", `gotm.exe`, `C:gotm.exe`, `\gotm.exe`, `C:\gotm.exe" --other`, "C:\\gotm\x00.exe", "C:\\gotm\n.exe", "C:\\gotm\r.exe", "C:\\gotm\t.exe", `C:\Apps\`, `C:\Apps\gotm.exe:stream`, `\\?\C:\Apps\gotm.exe`, `\\.\C:\Apps\gotm.exe`} {
		t.Run(fmt.Sprintf("%q", exe), func(t *testing.T) {
			if got, err := DebuggerCommand(exe); err == nil {
				t.Fatalf("unsafe executable accepted: %q => %q", exe, got)
			}
		})
	}
}

func TestAbsentRoutingIsNotCreatedByQueryRestoreOrRetarget(t *testing.T) {
	s := scratchStore(t)
	requireState(t, s, testExe, State{})
	mustSet(t, s.Restore(testExe))
	changed, err := s.Retarget(testExe, newExe)
	if changed || err != nil {
		t.Fatalf("absent Retarget = %v, %v", changed, err)
	}
	if k, err := registry.OpenKey(s.Root, s.Path, registry.QUERY_VALUE|registry.WOW64_64KEY); !errors.Is(err, registry.ErrNotExist) {
		k.Close()
		t.Fatalf("read/no-op created routing key: %v", err)
	}
}

func TestEnableRestoreAreIdempotentAndPreserveUnrelatedData(t *testing.T) {
	s := scratchStore(t)
	k := scratchKey(t, s)
	mustSet(t, k.SetStringValue("Unrelated", "keep me"))
	mustSet(t, k.SetDWordValue("UseFilter", 0))
	sub, _, err := registry.CreateKey(k, "FilterOne", registry.ALL_ACCESS|registry.WOW64_64KEY)
	mustSet(t, err)
	defer sub.Close()
	mustSet(t, sub.SetStringValue("FilterFullPath", `C:\Windows\System32\taskmgr.exe`))
	for range 2 {
		mustSet(t, s.Enable(testExe))
		requireState(t, s, testExe, State{Owned: true, Command: testCommand})
	}
	value, typ, err := k.GetStringValue("Debugger")
	if err != nil || typ != registry.SZ || value != testCommand {
		t.Fatalf("Debugger = %q, type %v, %v", value, typ, err)
	}
	for range 2 {
		mustSet(t, s.Restore(testExe))
		requireState(t, s, testExe, State{})
	}
	value, _, err = k.GetStringValue("Unrelated")
	if err != nil || value != "keep me" {
		t.Fatalf("unrelated value changed: %q, %v", value, err)
	}
	filter, _, err := k.GetIntegerValue("UseFilter")
	if err != nil || filter != 0 {
		t.Fatalf("UseFilter changed: %v, %v", filter, err)
	}
	value, _, err = sub.GetStringValue("FilterFullPath")
	if err != nil || value != `C:\Windows\System32\taskmgr.exe` {
		t.Fatalf("filter subkey changed: %q, %v", value, err)
	}
}

func TestEnableCreatesAbsentRouting(t *testing.T) {
	s := scratchStore(t)
	mustSet(t, s.Enable(testExe))
	requireState(t, s, testExe, State{Owned: true, Command: testCommand})
}

func TestOwnershipMatchesOnlyPathCaseAndExactArguments(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		owned         bool
	}{
		{"exact", testCommand, true},
		{"path case", `"c:\program files\GOTM\GOTM.EXE" --task-manager --`, true},
		{"argument case", `"C:\Program Files\gotm\gotm.exe" --TASK-MANAGER --`, false},
		{"extra argument", testCommand + " --elevated", false},
		{"no boundary", `"C:\Program Files\gotm\gotm.exe" --task-manager`, false},
		{"extra spacing", `"C:\Program Files\gotm\gotm.exe"  --task-manager --`, false},
		{"unquoted", `C:\Program Files\gotm\gotm.exe --task-manager --`, false},
		{"foreign", `"C:\Tools\other.exe"`, false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scratchStore(t)
			k := scratchKey(t, s)
			mustSet(t, k.SetStringValue("Debugger", tc.command))
			requireState(t, s, testExe, State{Owned: tc.owned, Conflict: !tc.owned, Command: tc.command})
			if tc.owned {
				mustSet(t, s.Restore(testExe))
				requireState(t, s, testExe, State{})
				return
			}
			if err := s.Enable(testExe); err == nil {
				t.Error("Enable accepted foreign routing")
			}
			mustSet(t, s.Restore(testExe))
			changed, err := s.Retarget(testExe, newExe)
			if changed || err != nil {
				t.Errorf("foreign Retarget = %v, %v; want false, nil", changed, err)
			}
			requireState(t, s, testExe, State{Conflict: true, Command: tc.command})
		})
	}
}

// Raw values exercise malformed registry data which GetStringValue silently
// truncates or converts. They must never be mistaken for an owned command.
func rawValue(t *testing.T, k registry.Key, name string, typ uint32, data []byte) {
	t.Helper()
	n, err := windows.NewNTUnicodeString(name)
	mustSet(t, err)
	var p *byte
	if len(data) > 0 {
		p = &data[0]
	}
	// RegSetValueExW repairs a missing REG_SZ terminator on supported Windows;
	// NtSetValueKey lets this fixture preserve deliberately malformed bytes.
	r, _, _ := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtSetValueKey").Call(uintptr(k), uintptr(unsafe.Pointer(n)), 0, uintptr(typ), uintptr(unsafe.Pointer(p)), uintptr(len(data)))
	if r != 0 {
		t.Fatalf("NtSetValueKey: %v", windows.NTStatus(r))
	}
}

func utf16Bytes(s string) []byte {
	words := utf16.Encode([]rune(s))
	data := make([]byte, 2*len(words))
	for i, word := range words {
		binary.LittleEndian.PutUint16(data[2*i:], word)
	}
	return data
}

func TestTypedAndMalformedDebuggerValuesArePreservedAsConflicts(t *testing.T) {
	validBytes := utf16Bytes(testCommand + "\x00")
	for _, tc := range []struct {
		name string
		typ  uint32
		data []byte
	}{
		{"expand string", registry.EXPAND_SZ, validBytes},
		{"binary", registry.BINARY, validBytes},
		{"integer", registry.DWORD, []byte{0, 0, 0, 0}},
		{"empty bytes", registry.SZ, nil},
		{"missing terminator", registry.SZ, utf16Bytes(testCommand)},
		{"odd bytes", registry.SZ, append(validBytes, 1)},
		{"embedded null", registry.SZ, utf16Bytes(testCommand + "\x00other\x00")},
		{"extra terminator", registry.SZ, append(append([]byte{}, validBytes...), 0, 0)},
		{"invalid surrogate", registry.SZ, []byte{0, 0xd8, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scratchStore(t)
			k := scratchKey(t, s)
			rawValue(t, k, "Debugger", tc.typ, tc.data)
			state, err := s.Query(testExe)
			if err != nil || !state.Conflict || state.Owned {
				t.Fatalf("Query malformed Debugger = %+v, %v", state, err)
			}
			if err := s.Enable(testExe); err == nil {
				t.Error("Enable accepted malformed Debugger")
			}
			mustSet(t, s.Restore(testExe))
			changed, err := s.Retarget(testExe, newExe)
			if changed || err != nil {
				t.Fatalf("malformed Retarget = %v, %v", changed, err)
			}
			buf := make([]byte, len(tc.data)+2)
			n, typ, err := k.GetValue("Debugger", buf)
			if err != nil || typ != tc.typ || string(buf[:n]) != string(tc.data) {
				t.Fatalf("malformed Debugger was changed: type %v, %x, %v", typ, buf[:n], err)
			}
		})
	}
}

func TestActiveAndUnsupportedFiltersBlockEnableAndRetarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  uint32
		data []byte
	}{
		{"active", registry.DWORD, []byte{1, 0, 0, 0}},
		{"unsupported active", registry.DWORD, []byte{2, 0, 0, 0}},
		{"qword zero", registry.QWORD, make([]byte, 8)},
		{"string zero", registry.SZ, utf16Bytes("0\x00")},
		{"malformed dword", registry.DWORD, []byte{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scratchStore(t)
			k := scratchKey(t, s)
			rawValue(t, k, "UseFilter", tc.typ, tc.data)
			requireState(t, s, testExe, State{Conflict: true})
			if err := s.Enable(testExe); err == nil {
				t.Error("Enable accepted conflicting UseFilter")
			}
			mustSet(t, k.SetStringValue("Debugger", testCommand))
			requireState(t, s, testExe, State{Owned: true, Conflict: true, Command: testCommand})
			changed, err := s.Retarget(testExe, newExe)
			if changed || err == nil {
				t.Fatalf("filtered Retarget = %v, %v; want false and error", changed, err)
			}
			mustSet(t, s.Restore(testExe))
			requireState(t, s, testExe, State{Conflict: true})
		})
	}
}

func TestRetargetMovesOwnedRoutingAndOldRestorePreservesNewOwner(t *testing.T) {
	s := scratchStore(t)
	mustSet(t, s.Enable(testExe))
	changed, err := s.Retarget(strings.ToLower(testExe), newExe)
	if !changed || err != nil {
		t.Fatalf("owned Retarget = %v, %v", changed, err)
	}
	requireState(t, s, newExe, State{Owned: true, Command: newCommand})
	mustSet(t, s.Restore(testExe))
	for _, old := range []string{testExe, newExe, strings.ToLower(newExe)} {
		changed, err = s.Retarget(old, newExe)
		if changed || err != nil {
			t.Errorf("repeated/same-target Retarget(%q) = %v, %v", old, changed, err)
		}
	}
	requireState(t, s, newExe, State{Owned: true, Command: newCommand})
}

func TestInvalidPathsAndRegistryReadErrorsPropagate(t *testing.T) {
	s := scratchStore(t)
	if _, err := s.Query("relative.exe"); err == nil {
		t.Error("Query accepted relative path")
	}
	if err := s.Enable("relative.exe"); err == nil {
		t.Error("Enable accepted relative path")
	}
	if err := s.Restore("relative.exe"); err == nil {
		t.Error("Restore accepted relative path")
	}
	if _, err := s.Retarget(testExe, "relative.exe"); err == nil {
		t.Error("Retarget accepted relative new path")
	}
	if _, err := s.Retarget("relative.exe", testExe); err == nil {
		t.Error("Retarget accepted relative old path")
	}
	s.Root = registry.Key(0)
	if _, err := s.Query(testExe); err == nil {
		t.Error("Query suppressed invalid registry handle")
	}
	if err := s.Enable(testExe); err == nil {
		t.Error("Enable suppressed invalid registry handle")
	}
	if err := s.Restore(testExe); err == nil {
		t.Error("Restore suppressed invalid registry handle")
	}
	if _, err := s.Retarget(testExe, newExe); err == nil {
		t.Error("Retarget suppressed invalid registry handle")
	}
}

func denyScratchWrites(t *testing.T, k registry.Key) {
	t.Helper()
	// Preserve an already-open ALL_ACCESS handle to repair only this scratch
	// key's ACL during cleanup. New handles must respect the read-only DACL.
	t.Cleanup(func() {
		mustSet(t, windows.SetSecurityInfo(windows.Handle(k), windows.SE_REGISTRY_KEY, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil))
	})
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;KR;;;WD)")
	mustSet(t, err)
	dacl, _, err := sd.DACL()
	mustSet(t, err)
	mustSet(t, windows.SetSecurityInfo(windows.Handle(k), windows.SE_REGISTRY_KEY, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))
}

func TestReadOnlyForeignAndAbsentRoutingAreNoOps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		foreign bool
	}{
		{"absent Debugger", false},
		{"foreign Debugger", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scratchStore(t)
			k := scratchKey(t, s)
			want := State{}
			if tc.foreign {
				mustSet(t, k.SetStringValue("Debugger", newCommand))
				want = State{Conflict: true, Command: newCommand}
			}
			denyScratchWrites(t, k)
			requireState(t, s, testExe, want)
			if err := s.Restore(testExe); err != nil {
				t.Errorf("read-only unowned Restore = %v; want nil", err)
			}
			changed, err := s.Retarget(testExe, newExe)
			if changed || err != nil {
				t.Fatalf("read-only unowned Retarget = %v, %v; want false, nil", changed, err)
			}
			requireState(t, s, testExe, want)
		})
	}
}

func TestDeniedWritesAreReportedWithoutChangingRouting(t *testing.T) {
	s := scratchStore(t)
	k := scratchKey(t, s)
	mustSet(t, k.SetStringValue("Debugger", testCommand))
	denyScratchWrites(t, k)
	requireState(t, s, testExe, State{Owned: true, Command: testCommand})
	if err := s.Enable(testExe); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("Enable denied-write error = %v", err)
	}
	if err := s.Restore(testExe); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("Restore denied-write error = %v", err)
	}
	changed, err := s.Retarget(testExe, newExe)
	if changed || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("Retarget denied-write = %v, %v", changed, err)
	}
	requireState(t, s, testExe, State{Owned: true, Command: testCommand})
}
