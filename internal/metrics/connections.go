//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIphlpapi           = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTbl = modIphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTbl = modIphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	afINET                = 2
	afINET6               = 23
	tcpTableOwnerPIDAll   = 5
	udpTableOwnerPID      = 1
	errInsufficientBuffer = 122
)

// Connection is one TCP/UDP endpoint with its owning process.
type Connection struct {
	Proto      string `json:"proto"` // TCP, TCP6, UDP, UDP6
	Local      string `json:"local"`
	LocalPort  int    `json:"localPort"`
	Remote     string `json:"remote"`
	RemotePort int    `json:"remotePort"`
	State      string `json:"state"`
	PID        uint32 `json:"pid"`
	// Exposure classifies who can reach a listening socket:
	// "Network" (any interface), "Local only" (loopback), "" (not listening).
	Exposure string `json:"exposure"`
}

var tcpStates = map[uint32]string{
	1: "Closed", 2: "Listen", 3: "SYN sent", 4: "SYN received", 5: "Established",
	6: "FIN wait 1", 7: "FIN wait 2", 8: "Close wait", 9: "Closing", 10: "Last ACK",
	11: "Time wait", 12: "Delete TCB",
}

func fetchTable(proc *windows.LazyProc, family, class uintptr) []byte {
	var size uint32
	proc.Call(0, uintptr(unsafe.Pointer(&size)), 1, family, class, 0)
	for try := 0; try < 4 && size > 0; try++ {
		buf := make([]byte, size+4096)
		sz := uint32(len(buf))
		r, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&sz)), 1, family, class, 0)
		if r == 0 {
			return buf[:sz]
		}
		if r != errInsufficientBuffer {
			return nil
		}
		size = sz
	}
	return nil
}

func port(v uint32) int { return int(v&0xFF)<<8 | int(v>>8&0xFF) } // network byte order

func ip4(b []byte) string { return net.IP(b[:4]).String() }
func ip6(b []byte) string { return net.IP(b[:16]).String() }

func exposure(addr string, listening bool) string {
	if !listening {
		return ""
	}
	ip := net.ParseIP(addr)
	if ip != nil && ip.IsLoopback() {
		return "Local only"
	}
	return "Network"
}

// ReadConnections returns every TCP and UDP endpoint (IPv4 and IPv6).
func ReadConnections() []Connection {
	le := binary.LittleEndian
	var out []Connection

	if b := fetchTable(procGetExtendedTcpTbl, afINET, tcpTableOwnerPIDAll); len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*24 <= len(b); i++ {
			r := b[4+i*24:]
			st := tcpStates[le.Uint32(r)]
			c := Connection{Proto: "TCP", Local: ip4(r[4:]), LocalPort: port(le.Uint32(r[8:])),
				Remote: ip4(r[12:]), RemotePort: port(le.Uint32(r[16:])), State: st, PID: le.Uint32(r[20:])}
			if st == "Listen" {
				c.Remote, c.RemotePort = "", 0
			}
			c.Exposure = exposure(c.Local, st == "Listen")
			out = append(out, c)
		}
	}
	if b := fetchTable(procGetExtendedTcpTbl, afINET6, tcpTableOwnerPIDAll); len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*56 <= len(b); i++ {
			r := b[4+i*56:]
			st := tcpStates[le.Uint32(r[48:])]
			c := Connection{Proto: "TCP6", Local: ip6(r), LocalPort: port(le.Uint32(r[20:])),
				Remote: ip6(r[24:]), RemotePort: port(le.Uint32(r[44:])), State: st, PID: le.Uint32(r[52:])}
			if st == "Listen" {
				c.Remote, c.RemotePort = "", 0
			}
			c.Exposure = exposure(c.Local, st == "Listen")
			out = append(out, c)
		}
	}
	if b := fetchTable(procGetExtendedUdpTbl, afINET, udpTableOwnerPID); len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*12 <= len(b); i++ {
			r := b[4+i*12:]
			c := Connection{Proto: "UDP", Local: ip4(r), LocalPort: port(le.Uint32(r[4:])),
				State: "—", PID: le.Uint32(r[8:])}
			c.Exposure = exposure(c.Local, true)
			out = append(out, c)
		}
	}
	if b := fetchTable(procGetExtendedUdpTbl, afINET6, udpTableOwnerPID); len(b) >= 4 {
		n := int(le.Uint32(b))
		for i := 0; i < n && 4+(i+1)*28 <= len(b); i++ {
			r := b[4+i*28:]
			c := Connection{Proto: "UDP6", Local: ip6(r), LocalPort: port(le.Uint32(r[20:])),
				State: "—", PID: le.Uint32(r[24:])}
			c.Exposure = exposure(c.Local, true)
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Proto != out[j].Proto {
			return out[i].Proto < out[j].Proto
		}
		if out[i].LocalPort != out[j].LocalPort {
			return out[i].LocalPort < out[j].LocalPort
		}
		return out[i].Remote < out[j].Remote
	})
	return out
}

// Endpoint formats address:port, bracketing IPv6 addresses.
func Endpoint(addr string, p int) string {
	if addr == "" {
		return ""
	}
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return fmt.Sprintf("[%s]:%d", addr, p)
	}
	return fmt.Sprintf("%s:%d", addr, p)
}
