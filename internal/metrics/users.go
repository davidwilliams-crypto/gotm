//go:build windows

package metrics

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modWtsapi32         = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSEnumSessions = modWtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySession = modWtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory   = modWtsapi32.NewProc("WTSFreeMemory")
)

const (
	wtsUserName   = 5
	wtsDomainName = 7
	wtsClientName = 10
)

var wtsStates = map[uint32]string{
	0: "Active", 1: "Connected", 2: "Connect query", 3: "Shadow", 4: "Disconnected",
	5: "Idle", 6: "Listening", 7: "Reset", 8: "Down", 9: "Init",
}

// UserSession is one Windows logon session.
type UserSession struct {
	ID      uint32 `json:"id"`
	User    string `json:"user"`
	State   string `json:"state"`
	Station string `json:"station"`
	Client  string `json:"client"`
}

type wtsSessionInfo struct {
	SessionID uint32
	_         uint32
	Station   *uint16
	State     uint32
	_         uint32
}

func wtsString(session uint32, class uintptr) string {
	var p *uint16
	var n uint32
	r, _, _ := procWTSQuerySession.Call(0, uintptr(session), class,
		uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || p == nil {
		return ""
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(p)))
	return windows.UTF16PtrToString(p)
}

// ReadSessions lists logon sessions via the Remote Desktop Services API.
func ReadSessions() []UserSession {
	var info *wtsSessionInfo
	var count uint32
	r, _, _ := procWTSEnumSessions.Call(0, 0, 1, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&count)))
	if r == 0 || info == nil {
		return nil
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(info)))

	var out []UserSession
	for _, s := range unsafe.Slice(info, count) {
		user := wtsString(s.SessionID, wtsUserName)
		dom := wtsString(s.SessionID, wtsDomainName)
		if user != "" && dom != "" {
			user = dom + `\` + user
		}
		if user == "" && s.SessionID == 0 {
			user = "Services"
		}
		out = append(out, UserSession{
			ID: s.SessionID, User: user, State: wtsStates[s.State],
			Station: windows.UTF16PtrToString(s.Station), Client: wtsString(s.SessionID, wtsClientName),
		})
	}
	return out
}

// ElevatedAs reports the account an elevated gotm runs as when it differs from
// the person signed in to this session (a standard user who typed an admin's
// credentials). "Current user" startup entries, per-user apps and settings
// then belong to that admin account, which the UI says.
func ElevatedAs() (account string, other bool) {
	if !IsElevated() {
		return "", false
	}
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", false
	}
	acct, _, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return "", false
	}
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil {
		return acct, false
	}
	const wtsUserName = 5
	signedIn := wtsString(session, wtsUserName)
	return acct, signedIn != "" && !strings.EqualFold(signedIn, acct)
}
