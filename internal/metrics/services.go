//go:build windows

package metrics

import (
	"fmt"
	"sort"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Service is one Windows service as shown in the Services tab.
type Service struct {
	Name        string `json:"name"`
	Display     string `json:"display"`
	State       string `json:"state"`
	StartType   string `json:"startType"`
	PID         uint32 `json:"pid"`
	CanStop     bool   `json:"canStop"`
	Path        string `json:"path,omitempty"`
	StartTypeOK bool   `json:"-"`
}

type svcCfg struct {
	start, path string
	at          time.Time
}

var (
	svcCfgMu    sync.Mutex
	svcCfgCache = map[string]svcCfg{}
)

const (
	scManagerConnect          = 0x0001
	scManagerEnumerateService = 0x0004
	scEnumProcessInfo         = 0
	serviceWin32              = 0x00000030
	serviceStateAll           = 0x00000003
	serviceQueryConfig        = 0x0001
	serviceQueryStatus        = 0x0004
	serviceStart              = 0x0010
	serviceStop               = 0x0020
	acceptStop                = 0x00000001
)

var svcStateNames = map[uint32]string{
	1: "Stopped", 2: "Starting", 3: "Stopping", 4: "Running",
	5: "Continuing", 6: "Pausing", 7: "Paused",
}

var svcStartNames = map[uint32]string{
	0: "Boot", 1: "System", 2: "Automatic", 3: "Manual", 4: "Disabled",
}

// ListServices enumerates Win32 services with state, PID and start type.
// It needs only SC_MANAGER_ENUMERATE_SERVICE, so it works without elevation.
func ListServices() ([]Service, error) {
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
		err = windows.EnumServicesStatusEx(scm, scEnumProcessInfo, serviceWin32, serviceStateAll,
			p, uint32(len(buf)), &needed, &returned, &resume, nil)
		if err == nil {
			break
		}
		if err != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("enumerate services: %w", err)
		}
		if needed == 0 {
			needed = 64 * 1024
		}
		buf = make([]byte, needed)
		resume = 0
	}

	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
	out := make([]Service, 0, returned)
	for _, e := range entries {
		name := windows.UTF16PtrToString(e.ServiceName)
		st := e.ServiceStatusProcess
		s := Service{
			Name:    name,
			Display: windows.UTF16PtrToString(e.DisplayName),
			State:   svcStateNames[st.CurrentState],
			PID:     st.ProcessId,
			CanStop: st.ControlsAccepted&acceptStop != 0 && st.CurrentState == 4,
		}
		s.StartType, s.Path = serviceConfig(scm, name)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Display < out[j].Display })
	return out, nil
}

func serviceConfig(scm windows.Handle, name string) (start, path string) {
	svcCfgMu.Lock()
	c, ok := svcCfgCache[name]
	svcCfgMu.Unlock()
	if ok && time.Since(c.at) < 2*time.Minute {
		return c.start, c.path
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", ""
	}
	h, err := windows.OpenService(scm, n, serviceQueryConfig)
	if err != nil {
		return "", ""
	}
	defer windows.CloseServiceHandle(h)
	var needed uint32
	_ = windows.QueryServiceConfig(h, nil, 0, &needed)
	if needed == 0 {
		return "", ""
	}
	buf := make([]byte, needed)
	cfg := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(h, cfg, needed, &needed); err != nil {
		return "", ""
	}
	start = svcStartNames[cfg.StartType]
	path = windows.UTF16PtrToString(cfg.BinaryPathName)
	svcCfgMu.Lock()
	svcCfgCache[name] = svcCfg{start: start, path: path, at: time.Now()}
	svcCfgMu.Unlock()
	return start, path
}

// ServiceControl starts, stops or restarts a service. Most services need an
// elevated token; the error from Windows is passed through unchanged.
func ServiceControl(name, action string) error {
	scm, err := windows.OpenSCManager(nil, nil, scManagerConnect)
	if err != nil {
		return fmt.Errorf("open service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	// Only the rights this action needs: a user allowed to start (but not stop)
	// a service must not get "access denied" for asking too much.
	access := uint32(serviceQueryStatus)
	switch action {
	case "start":
		access |= serviceStart
	case "stop":
		access |= serviceStop
	default:
		access |= serviceStart | serviceStop
	}
	h, err := windows.OpenService(scm, n, access)
	if err != nil {
		if err == windows.ERROR_ACCESS_DENIED {
			return fmt.Errorf("access denied for service %q (try Run as administrator)", name)
		}
		return fmt.Errorf("open service %q: %w", name, err)
	}
	s := &mgr.Service{Name: name, Handle: h}
	defer s.Close()

	switch action {
	case "start":
		return s.Start()
	case "stop":
		_, err := s.Control(svc.Stop)
		return err
	case "restart":
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			st, err := s.Query()
			if err != nil {
				return err
			}
			if st.State == svc.Stopped {
				return s.Start()
			}
			time.Sleep(250 * time.Millisecond)
		}
		return fmt.Errorf("service %q did not stop within 15 seconds", name)
	}
	return fmt.Errorf("unknown service action %q", action)
}
