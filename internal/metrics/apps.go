//go:build windows

package metrics

import (
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// InstalledApp is one entry from the Windows Uninstall registry keys.
type InstalledApp struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Publisher string `json:"publisher"`
	Installed string `json:"installed"` // YYYY-MM-DD when known
	SizeKB    uint64 `json:"sizeKb"`
	Location  string `json:"location"`
	Uninstall string `json:"uninstall"`
	// UninstallExpand is set when the value is REG_EXPAND_SZ (holds %VARS%).
	UninstallExpand bool   `json:"-"`
	Scope           string `json:"scope"` // Machine, Machine (32-bit), User
}

type uninstallRoot struct {
	root  registry.Key
	path  string
	scope string
}

var uninstallRoots = []uninstallRoot{
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "Machine"},
	{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, "Machine (32-bit)"},
	{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "User"},
}

// ReadInstalledApps lists installed programs, skipping hidden system
// components and update entries (no DisplayName, SystemComponent=1, ParentKeyName).
func ReadInstalledApps() []InstalledApp {
	seen := map[string]bool{}
	var out []InstalledApp
	for _, ur := range uninstallRoots {
		k, err := registry.OpenKey(ur.root, ur.path, registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			continue
		}
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			sk, err := registry.OpenKey(ur.root, ur.path+`\`+n, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			if name == "" {
				sk.Close()
				continue
			}
			if v, _, err := sk.GetIntegerValue("SystemComponent"); err == nil && v == 1 {
				sk.Close()
				continue
			}
			if p, _, _ := sk.GetStringValue("ParentKeyName"); p != "" {
				sk.Close()
				continue
			}
			a := InstalledApp{Name: strings.TrimSpace(name), Scope: ur.scope}
			a.Version, _, _ = sk.GetStringValue("DisplayVersion")
			a.Publisher, _, _ = sk.GetStringValue("Publisher")
			a.Location, _, _ = sk.GetStringValue("InstallLocation")
			var vt uint32
			a.Uninstall, vt, _ = sk.GetStringValue("UninstallString")
			a.UninstallExpand = vt == registry.EXPAND_SZ
			if d, _, _ := sk.GetStringValue("InstallDate"); len(d) == 8 {
				// yyyymmdd; some installers write yyyyddmm, so reject impossible months
				if m := d[4:6]; m >= "01" && m <= "12" {
					a.Installed = d[:4] + "-" + m + "-" + d[6:]
				}
			}
			if v, _, err := sk.GetIntegerValue("EstimatedSize"); err == nil {
				a.SizeKB = v
			}
			sk.Close()

			key := strings.ToLower(a.Name + "|" + a.Version)
			if seen[key] {
				continue // the same app often appears in both 64- and 32-bit hives
			}
			seen[key] = true
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
