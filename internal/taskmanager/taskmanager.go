//go:build windows

// Package taskmanager manages gotm's opt-in Windows Task Manager routing.
package taskmanager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows/registry"
)

// Store identifies the taskmgr.exe IFEO key. Supplying a different root/path
// allows callers' tests to use isolated HKCU keys without machine writes.
type Store struct {
	Root registry.Key
	Path string
}

// State describes routing relative to one executable. Owned and Conflict can
// both be true when our Debugger is present alongside an active path filter.
type State struct {
	Owned    bool
	Conflict bool
	Command  string
}

const commandSuffix = `" --task-manager --`

// MachineStore uses Windows' shared IFEO location. On supported Windows
// versions IFEO is shared across registry views; never write Wow6432Node.
func MachineStore() Store {
	return Store{
		Root: registry.LOCAL_MACHINE,
		Path: `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Image File Execution Options\taskmgr.exe`,
	}
}

// DebuggerCommand creates a command whose fixed argument boundary isolates
// Task Manager's Windows-appended arguments from gotm's own options. The setup
// helper separately enforces installation location and destination security.
func DebuggerCommand(exe string) (string, error) {
	if !utf8.ValidString(exe) || !filepath.IsAbs(exe) || strings.HasPrefix(exe, `\\?\`) || strings.HasPrefix(exe, `\\.\`) || strings.HasSuffix(exe, `\`) || strings.HasSuffix(exe, "/") {
		return "", errors.New("Task Manager replacement requires an absolute executable path")
	}
	if strings.ContainsAny(exe, `"<>|?*`) || strings.Contains(exe[len(filepath.VolumeName(exe)):], ":") || strings.ContainsFunc(exe, unicode.IsControl) {
		return "", errors.New("Task Manager replacement executable contains unsafe path characters")
	}
	return `"` + exe + commandSuffix, nil
}

// Query reads routing without creating or modifying any registry key.
func (s Store) Query(exe string) (State, error) {
	command, err := DebuggerCommand(exe)
	if err != nil {
		return State{}, err
	}
	return s.queryCommand(command)
}

func (s Store) queryCommand(command string) (State, error) {
	k, err := registry.OpenKey(s.Root, s.Path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read Task Manager replacement: %w", err)
	}
	defer k.Close()
	return queryKey(k, command)
}

// Enable installs routing only when no foreign Debugger or path filtering is
// configured. All writes use REG_SZ and preserve other values and subkeys.
func (s Store) Enable(exe string) error {
	command, err := DebuggerCommand(exe)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(s.Root, s.Path, registry.QUERY_VALUE|registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return fmt.Errorf("open Task Manager replacement for enabling: %w", err)
	}
	defer k.Close()
	state, err := queryKey(k, command)
	if err != nil {
		return err
	}
	if state.Conflict {
		return errors.New("Task Manager replacement conflicts with an existing Debugger or UseFilter value")
	}
	if state.Owned {
		return nil
	}
	if err := k.SetStringValue("Debugger", command); err != nil {
		return fmt.Errorf("enable Task Manager replacement: %w", err)
	}
	return nil
}

// Restore removes only this executable's exact owned Debugger. A foreign
// replacement is a no-op, including routing to a newer installation. An owned
// Debugger can be removed even if an independent UseFilter value has changed.
func (s Store) Restore(exe string) error {
	command, err := DebuggerCommand(exe)
	if err != nil {
		return err
	}
	// Uninstall needs no write permission when another program owns routing.
	state, err := s.queryCommand(command)
	if err != nil || !state.Owned {
		return err
	}
	k, err := registry.OpenKey(s.Root, s.Path, registry.QUERY_VALUE|registry.SET_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Task Manager replacement for restoring: %w", err)
	}
	defer k.Close()
	state, err = queryKey(k, command)
	if err != nil {
		return err
	}
	if !state.Owned {
		return nil
	}
	if err := k.DeleteValue("Debugger"); err != nil {
		return fmt.Errorf("restore Windows Task Manager: %w", err)
	}
	return nil
}

// Retarget changes routing only while oldExe still owns it. Callers must first
// place and validate the new protected executable. False means no write was
// made; in particular absent/foreign routing is left untouched. The reverse
// call can conditionally roll back an installation metadata failure.
func (s Store) Retarget(oldExe, newExe string) (bool, error) {
	oldCommand, err := DebuggerCommand(oldExe)
	if err != nil {
		return false, err
	}
	newCommand, err := DebuggerCommand(newExe)
	if err != nil {
		return false, err
	}
	// Relocation leaves absent or foreign routing alone without requesting
	// write access. Ownership is checked again on the mutation handle below.
	state, err := s.queryCommand(oldCommand)
	if err != nil || !state.Owned {
		return false, err
	}
	k, err := registry.OpenKey(s.Root, s.Path, registry.QUERY_VALUE|registry.SET_VALUE|registry.WOW64_64KEY)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open Task Manager replacement for retargeting: %w", err)
	}
	defer k.Close()
	state, err = queryKey(k, oldCommand)
	if err != nil || !state.Owned {
		return false, err
	}
	if state.Conflict {
		return false, errors.New("Task Manager replacement conflicts with an existing UseFilter value")
	}
	if ownsCommand(state.Command, newCommand) {
		return false, nil
	}
	if err := k.SetStringValue("Debugger", newCommand); err != nil {
		return false, fmt.Errorf("retarget Task Manager replacement: %w", err)
	}
	return true, nil
}

// Read ownership again using the same handle that will perform any mutation.
// Registry values have no compare-and-swap primitive; external administrators
// must coordinate concurrent IFEO changes with installation maintenance.
func queryKey(k registry.Key, command string) (State, error) {
	var state State
	data, typ, err := readValue(k, "Debugger")
	if err == nil {
		value, valid := registryString(data)
		if valid {
			state.Command = value
		}
		state.Owned = typ == registry.SZ && valid && ownsCommand(value, command)
		state.Conflict = !state.Owned
	} else if !errors.Is(err, registry.ErrNotExist) {
		return State{}, fmt.Errorf("read Task Manager Debugger: %w", err)
	}
	data, typ, err = readValue(k, "UseFilter")
	if err == nil {
		if typ != registry.DWORD || len(data) != 4 || binary.LittleEndian.Uint32(data) != 0 {
			state.Conflict = true
		}
	} else if !errors.Is(err, registry.ErrNotExist) {
		return State{}, fmt.Errorf("read Task Manager UseFilter: %w", err)
	}
	return state, nil
}

func ownsCommand(value, command string) bool {
	if !strings.HasSuffix(value, commandSuffix) || !strings.HasPrefix(value, `"`) {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(value, commandSuffix), strings.TrimSuffix(command, commandSuffix))
}

func readValue(k registry.Key, name string) ([]byte, uint32, error) {
	buf := make([]byte, 256)
	for {
		n, typ, err := k.GetValue(name, buf)
		if err == nil {
			return buf[:n], typ, nil
		}
		if !errors.Is(err, registry.ErrShortBuffer) || n <= len(buf) {
			return nil, typ, err
		}
		buf = make([]byte, n)
	}
}

// REG_SZ must be well-formed UTF-16 with exactly one trailing terminator.
// GetStringValue also accepts REG_EXPAND_SZ and silently truncates malformed
// strings, either of which could incorrectly establish ownership here.
func registryString(data []byte) (string, bool) {
	if len(data) < 2 || len(data)%2 != 0 || binary.LittleEndian.Uint16(data[len(data)-2:]) != 0 {
		return "", false
	}
	words := make([]uint16, len(data)/2-1)
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(data[2*i:])
		if words[i] == 0 {
			return "", false
		}
	}
	for i := 0; i < len(words); i++ {
		switch {
		case words[i] >= 0xd800 && words[i] <= 0xdbff:
			if i+1 >= len(words) || words[i+1] < 0xdc00 || words[i+1] > 0xdfff {
				return "", false
			}
			i++
		case words[i] >= 0xdc00 && words[i] <= 0xdfff:
			return "", false
		}
	}
	return string(utf16.Decode(words)), true
}
