//go:build windows

package metrics

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// StartupItem is one program that starts at logon or boot.
type StartupItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	User    string `json:"user,omitempty"`
}

const (
	runKey      = `Software\Microsoft\Windows\CurrentVersion\Run`
	runKey32    = `Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Run`
	approvedRun = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
	approved32  = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run32`
	approvedDir = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\StartupFolder`
)

type regSource struct {
	label    string
	root     registry.Key
	rootName string
	key      string
	approved string
}

var regSources = []regSource{
	{"Registry (current user)", registry.CURRENT_USER, "HKCU", runKey, approvedRun},
	{"Registry (all users)", registry.LOCAL_MACHINE, "HKLM", runKey, approvedRun},
	{"Registry (all users, 32-bit)", registry.LOCAL_MACHINE, "HKLM32", runKey32, approved32},
}

// ListStartup gathers registry Run entries, Startup-folder items and
// logon/boot scheduled tasks, with their enabled state from StartupApproved.
func ListStartup() []StartupItem {
	var out []StartupItem

	for _, src := range regSources {
		k, err := registry.OpenKey(src.root, src.key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _ := k.ReadValueNames(0)
		for _, n := range names {
			v, _, err := k.GetStringValue(n)
			if err != nil {
				continue
			}
			out = append(out, StartupItem{
				ID: "reg|" + src.rootName + "|" + n, Name: n, Command: v,
				Source: src.label, Enabled: approvedState(src.root, src.approved, n),
			})
		}
		k.Close()
	}

	dirs := []struct {
		label string
		root  registry.Key
		path  string
		id    string
	}{
		// from the shell, not %APPDATA%/%ProgramData%, which a user can redefine
		{"Startup folder (current user)", registry.CURRENT_USER, knownFolder(windows.FOLDERID_Startup), "dirU"},
		{"Startup folder (all users)", registry.LOCAL_MACHINE, knownFolder(windows.FOLDERID_CommonStartup), "dirM"},
	}
	for _, d := range dirs {
		ents, err := os.ReadDir(d.path)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || strings.EqualFold(e.Name(), "desktop.ini") {
				continue
			}
			out = append(out, StartupItem{
				ID: d.id + "|" + e.Name(), Name: e.Name(),
				Command: filepath.Join(d.path, e.Name()), Source: d.label,
				Enabled: approvedState(d.root, approvedDir, e.Name()),
			})
		}
	}

	out = append(out, logonTasks()...)
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// approvedState reads the 12-byte StartupApproved value Explorer/Task Manager
// use: a first byte with its low bit clear (0x02, 0x06...) means enabled.
func approvedState(root registry.Key, key, name string) bool {
	k, err := registry.OpenKey(root, key, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	b, _, err := k.GetBinaryValue(name)
	if err != nil || len(b) == 0 {
		return true
	}
	return b[0]&1 == 0
}

// SetStartupEnabled flips the enabled state of a startup item by ID.
func SetStartupEnabled(id string, enabled bool) error {
	parts := strings.SplitN(id, "|", 3)
	switch parts[0] {
	case "reg":
		if len(parts) != 3 {
			break
		}
		for _, src := range regSources {
			if src.rootName == parts[1] {
				return writeApproved(src.root, src.approved, parts[2], enabled)
			}
		}
	case "dirU", "dirM":
		if len(parts) < 2 {
			break
		}
		root := registry.CURRENT_USER
		if parts[0] == "dirM" {
			root = registry.LOCAL_MACHINE
		}
		return writeApproved(root, approvedDir, strings.Join(parts[1:], "|"), enabled)
	case "task":
		if len(parts) < 2 {
			break
		}
		flag := "/disable"
		if enabled {
			flag = "/enable"
		}
		return runSchtasks("/change", "/tn", strings.Join(parts[1:], "|"), flag)
	}
	return fmt.Errorf("unknown startup item %q", id)
}

func writeApproved(root registry.Key, key, name string, enabled bool) error {
	k, _, err := registry.CreateKey(root, key, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("open %s: %w (try Run as administrator)", key, err)
	}
	defer k.Close()
	val := make([]byte, 12)
	if enabled {
		val[0] = 0x02
	} else {
		val[0] = 0x03
		ft := windows.NsecToFiletime(time.Now().UnixNano())
		binary.LittleEndian.PutUint32(val[4:], ft.LowDateTime)
		binary.LittleEndian.PutUint32(val[8:], ft.HighDateTime)
	}
	if err := k.SetBinaryValue(name, val); err != nil {
		return fmt.Errorf("write StartupApproved: %w", err)
	}
	return nil
}

func hiddenCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return c
}

func runSchtasks(args ...string) error {
	out, err := hiddenCmd(system32("schtasks.exe"), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// logonTasks lists scheduled tasks with an enabled logon or boot trigger. It
// reads `schtasks /query /xml ONE`: the task XML is the same in every Windows
// display language, unlike the CSV listing's translated, quote-fragile columns.
func logonTasks() []StartupItem {
	out, err := hiddenCmd(system32("schtasks.exe"), "/query", "/xml", "ONE").Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return parseTaskXML(out)
}

type xmlTrigger struct {
	Enabled string `xml:"Enabled"`
	UserID  string `xml:"UserId"`
}

type xmlTask struct {
	Settings struct {
		Enabled string `xml:"Enabled"`
	} `xml:"Settings"`
	Triggers struct {
		Logon []xmlTrigger `xml:"LogonTrigger"`
		Boot  []xmlTrigger `xml:"BootTrigger"`
	} `xml:"Triggers"`
	Actions struct {
		Exec []struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Exec"`
		COM []struct {
			ClassID string `xml:"ClassId"`
		} `xml:"ComHandler"`
	} `xml:"Actions"`
	Principals struct {
		Principal []struct {
			UserID  string `xml:"UserId"`
			GroupID string `xml:"GroupId"`
		} `xml:"Principal"`
	} `xml:"Principals"`
}

func parseTaskXML(out []byte) []StartupItem {
	if !utf8.Valid(out) {
		out = oemToUTF8(out) // schtasks writes the console code page when redirected
	}
	dec := xml.NewDecoder(bytes.NewReader(out))
	dec.Strict = false
	var items []StartupItem
	path := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.Comment:
			path = strings.TrimSpace(string(t)) // <!-- \Folder\Task name -->
		case xml.StartElement:
			if t.Name.Local != "Task" {
				continue
			}
			var x xmlTask
			if dec.DecodeElement(&x, &t) != nil || path == "" {
				continue
			}
			on := func(tr xmlTrigger) bool { return !strings.EqualFold(strings.TrimSpace(tr.Enabled), "false") }
			startup, user := false, ""
			for _, tr := range x.Triggers.Logon {
				if on(tr) {
					startup, user = true, tr.UserID
				}
			}
			for _, tr := range x.Triggers.Boot {
				if on(tr) {
					startup = true
				}
			}
			if !startup {
				continue
			}
			var cmd string
			if len(x.Actions.Exec) > 0 {
				cmd = strings.TrimSpace(x.Actions.Exec[0].Command + " " + x.Actions.Exec[0].Arguments)
			} else if len(x.Actions.COM) > 0 {
				cmd = "COM handler " + x.Actions.COM[0].ClassID
			}
			if user == "" && len(x.Principals.Principal) > 0 {
				user = x.Principals.Principal[0].UserID
				if user == "" {
					user = x.Principals.Principal[0].GroupID
				}
			}
			name := path
			if i := strings.LastIndex(path, `\`); i >= 0 && i+1 < len(path) {
				name = path[i+1:]
			}
			items = append(items, StartupItem{
				ID:      "task|" + path,
				Name:    name,
				Command: cmd,
				Source:  "Scheduled task",
				Enabled: !strings.EqualFold(strings.TrimSpace(x.Settings.Enabled), "false"),
				User:    accountName(user),
			})
			path = ""
		}
	}
	return items
}

// accountName turns a SID string into DOMAIN\name; other text is returned as is.
func accountName(s string) string {
	if !strings.HasPrefix(s, "S-1-") {
		return s
	}
	sid, err := windows.StringToSid(s)
	if err != nil {
		return s
	}
	acct, dom, _, err := sid.LookupAccount("")
	if err != nil {
		return s
	}
	if dom == "" {
		return acct
	}
	return dom + `\` + acct
}

// oemToUTF8 converts console code page bytes to UTF-8.
func oemToUTF8(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	const cpOEM = 1 // CP_OEMCP
	n, err := windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n == 0 {
		return b
	}
	w := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cpOEM, 0, &b[0], int32(len(b)), &w[0], n); err != nil {
		return b
	}
	return []byte(string(utf16.Decode(w)))
}

// system32 returns the full path of a Windows tool, so PATH order never decides
// which program runs.
func system32(exe string) string {
	if dir, err := windows.GetSystemDirectory(); err == nil {
		return filepath.Join(dir, exe)
	}
	return exe
}

func knownFolder(id *windows.KNOWNFOLDERID) string {
	p, err := windows.KnownFolderPath(id, 0)
	if err != nil {
		return ""
	}
	return p
}
