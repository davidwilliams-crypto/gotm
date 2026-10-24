//go:build windows

package metrics

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DriverInfo is one kernel / file-system driver service.
type DriverInfo struct {
	Name      string `json:"name"`
	Display   string `json:"display"`
	State     string `json:"state"`
	StartType string `json:"startType"`
	Kind      string `json:"kind"` // Kernel, File system, Recognizer
	Path      string `json:"path"`
	Company   string `json:"company"`
	Version   string `json:"version"`
	Desc      string `json:"description"`
}

var driverKinds = map[uint32]string{1: "Kernel", 2: "File system", 8: "Recognizer"}

// ReadDrivers enumerates driver services. File version details are filled in
// from each image (cached), which is what makes the list useful for spotting
// third-party or outdated drivers.
func ReadDrivers() ([]DriverInfo, error) {
	scm, err := windows.OpenSCManager(nil, nil, scManagerConnect|scManagerEnumerateService)
	if err != nil {
		return nil, fmt.Errorf("open service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	var needed, returned, resume uint32
	var buf []byte
	for {
		var p *byte
		if len(buf) > 0 {
			p = &buf[0]
		}
		err = windows.EnumServicesStatusEx(scm, scEnumProcessInfo, windows.SERVICE_DRIVER, serviceStateAll,
			p, uint32(len(buf)), &needed, &returned, &resume, nil)
		if err == nil {
			break
		}
		if err != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("enumerate drivers: %w", err)
		}
		if needed == 0 {
			needed = 128 * 1024
		}
		buf = make([]byte, needed)
		resume = 0
	}

	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
	out := make([]DriverInfo, 0, returned)
	for _, e := range entries {
		name := windows.UTF16PtrToString(e.ServiceName)
		st := e.ServiceStatusProcess
		d := DriverInfo{
			Name:    name,
			Display: windows.UTF16PtrToString(e.DisplayName),
			State:   svcStateNames[st.CurrentState],
			Kind:    driverKinds[st.ServiceType&0xB],
		}
		d.StartType, d.Path = serviceConfig(scm, name)
		d.Path = normalizeDriverPath(d.Path, name)
		out = append(out, d)
	}
	fillVersionInfo(out)
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// normalizeDriverPath turns the SCM's ImagePath forms into a normal path:
// "\SystemRoot\system32\drivers\x.sys", "\??\C:\...", "system32\drivers\x.sys".
func normalizeDriverPath(p, name string) string {
	root, err := windows.GetWindowsDirectory() // not %SystemRoot%, which a user can redefine
	if err != nil {
		root = `C:\Windows`
	}
	if root == "" {
		root = `C:\Windows`
	}
	switch {
	case p == "":
		return filepath.Join(root, "System32", "drivers", name+".sys")
	case strings.HasPrefix(strings.ToLower(p), `\systemroot\`):
		return filepath.Join(root, p[len(`\SystemRoot\`):])
	case strings.HasPrefix(p, `\??\`):
		return p[4:]
	case len(p) > 1 && p[1] != ':' && !strings.HasPrefix(p, `\`):
		return filepath.Join(root, p)
	}
	return p
}

var (
	verMu    sync.Mutex
	verCache = map[string][3]string{} // path -> company, version, description
)

func fillVersionInfo(ds []DriverInfo) {
	// bounded fan-out: reading 500 file headers is I/O bound, not CPU bound
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range ds {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, v, desc := FileVersion(ds[i].Path)
			ds[i].Company, ds[i].Version, ds[i].Desc = c, v, desc
		}()
	}
	wg.Wait()
}

// FileVersion returns CompanyName, FileVersion and FileDescription of a binary.
func FileVersion(path string) (company, version, desc string) {
	verMu.Lock()
	if v, ok := verCache[path]; ok {
		verMu.Unlock()
		return v[0], v[1], v[2]
	}
	verMu.Unlock()

	defer func() {
		verMu.Lock()
		verCache[path] = [3]string{company, version, desc}
		verMu.Unlock()
	}()

	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return
	}
	data := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&data[0])); err != nil {
		return
	}
	var tr *[2]uint16
	var l uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&tr), &l); err != nil || l < 4 {
		tr = &[2]uint16{0x0409, 0x04B0}
	}
	get := func(field string) string {
		sub := fmt.Sprintf(`\StringFileInfo\%04x%04x\%s`, tr[0], tr[1], field)
		var p *uint16
		var n uint32
		if err := windows.VerQueryValue(unsafe.Pointer(&data[0]), sub, unsafe.Pointer(&p), &n); err != nil || n == 0 {
			return ""
		}
		return strings.TrimSpace(windows.UTF16PtrToString(p))
	}
	return get("CompanyName"), get("FileVersion"), get("FileDescription")
}
