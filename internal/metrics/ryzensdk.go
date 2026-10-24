//go:build windows

package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// This file binds AMD's Ryzen Master Monitoring SDK (Platform.dll / Device.dll,
// read-only API: IPlatform, IDeviceManager, ICPUEx) so gotm can show Tctl/Tdie,
// per-core temperature and effective clocks, PPT/TDC/EDC and rail power.
//
// The SDK is a C++ interface, so objects are driven through their vtables. The
// slot numbers below follow the declaration order in the SDK's IPlatform.h,
// IDeviceManager.h, IDevice.h and ICPUEx.h (3.1.1). The SDK exposes no setters;
// nothing here can change a CPU setting.
//
// Safety properties:
//   - opt-in: nothing is loaded unless the user turns it on;
//   - the DLLs are located only through HKLM (admin-writable), never through a
//     per-user setting, and must carry a valid Authenticode signature;
//   - needs an elevated process (the SDK registers/uses AMD's signed driver);
//   - a marker turns the feature off after a crash during initialisation;
//   - the on/off switch and that marker live in HKLM\SOFTWARE\gotm, which only
//     administrators can write: gotm runs this elevated, so it must not take
//     the decision from, or write state into, the user's own profile.

// vtable slots (x64 MSVC: one pointer each, declaration order).
const (
	slotPlatformInit    = 0 // bool Init(const char*, bool)
	slotPlatformUnInit  = 1
	slotPlatformDevMgr  = 2 // IDeviceManager& GetIDeviceManager()
	slotMgrGetDevice    = 2 // IDevice* GetDevice(type, index) – first overload
	slotDeviceUnInit    = 1
	slotDeviceGetName   = 2
	slotDeviceGetType   = 7
	slotCPUGetCorePark  = 15 // int GetCorePark(unsigned int&)
	slotCPUGetPackage   = 16 // const wchar_t* GetPackage()
	slotCPUGetCPUParams = 17 // int GetCPUParameters(CPUParameters&)
)

const (
	dtCPU = 0

	ryzenParamsBuf = 512 // SDK struct is 192 bytes; leave room so a newer DLL cannot overrun us
	ryzenMaxCores  = 256
)

// ryzenParams mirrors the SDK's CPUParameters on x64 MSVC (192 bytes).
type ryzenParams struct {
	Mode       uint32
	_          uint32
	FreqLen    uint32
	_          uint32
	Freq       unsafe.Pointer // double[FreqLen] effective frequency, MHz
	State      unsafe.Pointer // double[FreqLen] C0 residency, %
	CurFreq    unsafe.Pointer // double[FreqLen] current frequency, MHz
	CurTemp    unsafe.Pointer // double[FreqLen] per-core temperature, °C
	PeakV      float64
	PeakV1     float64
	SocV       float64
	Temp       float64 // Tctl/Tdie, °C
	AvgV       float64
	AvgV1      float64
	PeakSpeed  float64
	PPTLimit   float32
	PPTValue   float32
	TDCLimVDD  float32
	TDCValVDD  float32
	TDCValVDD1 float32
	EDCLimVDD  float32
	EDCValVDD  float32
	EDCValVDD1 float32
	CHTCLimit  float32
	FCLK       float32
	CCLKFmax   float32
	TDCLimSOC  float32
	TDCValSOC  float32
	EDCLimSOC  float32
	EDCValSOC  float32
	VDDPower   float32
	SOCPower   float32
	TDCLimCCD  float32
	TDCValCCD  float32
	EDCLimCCD  float32
	EDCValCCD  float32
}

// RyzenCore is one core's effective readings.
type RyzenCore struct {
	Index  int
	EffMHz float64 // effective frequency (C0-weighted)
	CurMHz float64 // instantaneous frequency
	C0Pct  float64 // C0 residency
	TempC  float64
}

// RyzenReading is one GetCPUParameters sample.
type RyzenReading struct {
	Time                      time.Time
	TempC                     float64 // Tctl/Tdie
	PeakMHz                   float64
	CoreVPeak, CoreVAvg, SocV float64
	PPT, PPTLimit             float64 // package power, W
	TDCVDD, TDCVDDLimit       float64 // A
	EDCVDD, EDCVDDLimit       float64
	TDCSOC, TDCSOCLimit       float64
	EDCSOC, EDCSOCLimit       float64
	VDDPower, SOCPower        float64 // rail power, W
	TDCCCD, EDCCCD            float64
	CHTCLimit                 float64 // °C
	FCLKMHz, CCLKFmaxMHz      float64
	Cores                     []RyzenCore
}

// RyzenState says why a reading is or is not available.
type RyzenState int

const (
	RyzenOff RyzenState = iota
	RyzenStarting
	RyzenRunning
	RyzenUnavailable
)

// ryzenDirOverride lets tests point at an extracted SDK; never set in the app.
var ryzenDirOverride string

// RyzenSDK polls the Monitoring SDK on its own locked OS thread.
type RyzenSDK struct {
	marker regMarker

	mu     sync.Mutex
	state  RyzenState
	detail string
	last   RyzenReading
	have   bool
	cpu    string
	pkg    string

	stop chan struct{}
	done chan struct{}
}

// NewRyzenSDK prepares (but does not start) a poller. A value in
// HKLM\SOFTWARE\gotm detects a crash during initialisation.
func NewRyzenSDK() *RyzenSDK {
	return newRyzenSDKWithMarker(regMarker{registry.LOCAL_MACHINE, gotmKey, "RyzenSDKPending"})
}

func newRyzenSDKWithMarker(m regMarker) *RyzenSDK {
	return &RyzenSDK{marker: m, state: RyzenOff, detail: "Off"}
}

const gotmKey = `SOFTWARE\gotm`

// regMarker is one registry value used as a flag; the zero value is disabled.
type regMarker struct {
	root       registry.Key
	path, name string
}

func (m regMarker) on() bool {
	if m.path == "" {
		return false
	}
	k, err := registry.OpenKey(m.root, m.path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(m.name)
	return err == nil && v != 0
}

func (m regMarker) set(on bool) error {
	if m.path == "" {
		return nil
	}
	if !on {
		k, err := registry.OpenKey(m.root, m.path, registry.SET_VALUE)
		if err != nil {
			return nil // nothing to clear
		}
		defer k.Close()
		if err := k.DeleteValue(m.name); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	k, _, err := registry.CreateKey(m.root, m.path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue(m.name, 1)
}

// RyzenSDKEnabled reports the machine-wide switch an administrator set.
func RyzenSDKEnabled() bool {
	return regMarker{registry.LOCAL_MACHINE, gotmKey, "RyzenSDK"}.on()
}

// SetRyzenSDKEnabled stores the switch (administrator only) and clears any
// crash marker, so turning it on again is a fresh attempt.
func SetRyzenSDKEnabled(on bool) error {
	if err := (regMarker{registry.LOCAL_MACHINE, gotmKey, "RyzenSDK"}).set(on); err != nil {
		return err
	}
	return regMarker{registry.LOCAL_MACHINE, gotmKey, "RyzenSDKPending"}.set(false)
}

// Start begins polling in the background; it returns immediately.
func (r *RyzenSDK) Start() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	r.stop, r.done = make(chan struct{}), make(chan struct{})
	r.state, r.detail, r.have = RyzenStarting, "Starting…", false
	stop, done := r.stop, r.done
	r.mu.Unlock()
	go func() {
		defer close(done)
		r.run(stop)
	}()
}

// Stop ends polling and releases the SDK. Safe to call repeatedly.
func (r *RyzenSDK) Stop() {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.stop, r.done = nil, nil
	r.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
	r.mu.Lock()
	r.state, r.detail, r.have = RyzenOff, "Off", false
	r.mu.Unlock()
}

// State returns the current status and a one-line explanation.
func (r *RyzenSDK) State() (RyzenState, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.detail
}

// Reading returns the newest sample, if any.
func (r *RyzenSDK) Reading() (RyzenReading, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.have {
		return RyzenReading{}, false
	}
	out := r.last
	out.Cores = append([]RyzenCore(nil), r.last.Cores...)
	return out, true
}

// Identity returns the CPU name and package the SDK reported.
func (r *RyzenSDK) Identity() (cpu, pkg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cpu, r.pkg
}

func (r *RyzenSDK) fail(detail string) {
	r.mu.Lock()
	r.state, r.detail, r.have = RyzenUnavailable, detail, false
	r.mu.Unlock()
}

// ---- vtable plumbing ----------------------------------------------------------

func rawPtr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }

func peekPtr(addr uintptr) uintptr { return *(*uintptr)(rawPtr(addr)) }

// vcall invokes slot n of the C++ object at obj (this in RCX, MSVC x64 ABI).
// uintptrescapes keeps Go memory passed as uintptr(unsafe.Pointer(&x)) alive
// and on the heap for the call, as syscall.SyscallN's own callers get.
//
//go:uintptrescapes
func vcall(obj uintptr, slot int, args ...uintptr) uintptr {
	fn := peekPtr(peekPtr(obj) + uintptr(slot)*unsafe.Sizeof(uintptr(0)))
	a := make([]uintptr, 0, len(args)+1)
	a = append(a, obj)
	a = append(a, args...)
	ret, _, _ := syscall.SyscallN(fn, a...)
	return ret
}

func wstr(p uintptr) string {
	if p == 0 {
		return ""
	}
	var u []uint16
	for i := uintptr(0); i < 512; i++ {
		c := *(*uint16)(rawPtr(p + i*2))
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return strings.TrimSpace(windows.UTF16ToString(u))
}

// ---- locating and vetting the SDK --------------------------------------------

// ryzenSDKDir returns the SDK's bin directory from HKLM, which only an
// administrator can write.
func ryzenSDKDir() (string, error) {
	if ryzenDirOverride != "" {
		return ryzenDirOverride, nil
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\AMD\RyzenMasterMonitoringSDK`, registry.QUERY_VALUE)
	if err != nil {
		return "", errors.New("AMD Ryzen Master Monitoring SDK is not installed")
	}
	defer k.Close()
	p, _, err := k.GetStringValue("InstallationPath")
	if err != nil || p == "" {
		return "", errors.New("SDK install path missing from the registry")
	}
	return filepath.Join(p, "bin"), nil
}

// verifySigned runs WinVerifyTrust on one file (no UI, no network revocation).
func verifySigned(path string) error {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	data := &windows.WinTrustData{
		Size:             uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:         windows.WTD_UI_NONE,
		RevocationChecks: windows.WTD_REVOKE_NONE,
		UnionChoice:      windows.WTD_CHOICE_FILE,
		StateAction:      windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(&windows.WinTrustFileInfo{
			Size:     uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
			FilePath: p16,
		}),
	}
	verr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	return verr
}

// ---- decoding -----------------------------------------------------------------

func f64At(base unsafe.Pointer, i int) float64 {
	return *(*float64)(unsafe.Add(base, i*8))
}

// decodeRyzenParams turns the SDK's struct into a reading. corePark is the
// number of parked cores the SDK reports (they sit at the end of the arrays).
func decodeRyzenParams(p *ryzenParams, now time.Time) (RyzenReading, error) {
	if p.Temp <= 0 || p.Temp > 150 {
		return RyzenReading{}, fmt.Errorf("implausible temperature %.1f", p.Temp)
	}
	r := RyzenReading{
		Time: now, TempC: p.Temp, PeakMHz: p.PeakSpeed,
		CoreVPeak: p.PeakV, CoreVAvg: p.AvgV, SocV: p.SocV,
		PPT: float64(p.PPTValue), PPTLimit: float64(p.PPTLimit),
		TDCVDD: float64(p.TDCValVDD), TDCVDDLimit: float64(p.TDCLimVDD),
		EDCVDD: float64(p.EDCValVDD), EDCVDDLimit: float64(p.EDCLimVDD),
		TDCSOC: float64(p.TDCValSOC), TDCSOCLimit: float64(p.TDCLimSOC),
		EDCSOC: float64(p.EDCValSOC), EDCSOCLimit: float64(p.EDCLimSOC),
		VDDPower: float64(p.VDDPower), SOCPower: float64(p.SOCPower),
		TDCCCD: float64(p.TDCValCCD), EDCCCD: float64(p.EDCValCCD),
		CHTCLimit: float64(p.CHTCLimit),
		FCLKMHz:   float64(p.FCLK), CCLKFmaxMHz: float64(p.CCLKFmax),
	}
	n := int(p.FreqLen)
	if n < 0 || n > ryzenMaxCores {
		return RyzenReading{}, fmt.Errorf("implausible core count %d", n)
	}
	for i := 0; i < n; i++ {
		c := RyzenCore{Index: i}
		if p.Freq != nil {
			c.EffMHz = f64At(p.Freq, i)
		}
		if p.State != nil {
			c.C0Pct = f64At(p.State, i)
		}
		if p.CurFreq != nil {
			c.CurMHz = f64At(p.CurFreq, i)
		}
		if p.CurTemp != nil {
			c.TempC = f64At(p.CurTemp, i)
		}
		r.Cores = append(r.Cores, c)
	}
	return r, nil
}

// ---- worker --------------------------------------------------------------------

func (r *RyzenSDK) run(stop <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if r.marker.on() {
		r.fail("Disabled: the previous start-up of the AMD SDK did not finish. Turn it off and on again in Settings to retry.")
		return
	}
	if !IsElevated() {
		r.fail("Needs administrator rights (use “Run as administrator”).")
		return
	}
	dir, err := ryzenSDKDir()
	if err != nil {
		r.fail(err.Error())
		return
	}
	platformDLL := filepath.Join(dir, "Platform.dll")
	deviceDLL := filepath.Join(dir, "Device.dll")
	for _, f := range []string{platformDLL, deviceDLL} {
		if _, err := os.Stat(f); err != nil {
			r.fail("SDK file missing: " + f)
			return
		}
		if err := verifySigned(f); err != nil {
			r.fail(fmt.Sprintf("Refusing to load %s: signature check failed (%v)", filepath.Base(f), err))
			return
		}
		if err := signedByAMD(f); err != nil { // valid is not enough: it must be AMD's
			r.fail("Refusing to load " + filepath.Base(f) + ": " + err.Error())
			return
		}
	}

	_ = r.marker.set(true)
	h, err := windows.LoadLibraryEx(platformDLL, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		r.clearMarker()
		r.fail("Could not load Platform.dll: " + err.Error())
		return
	}
	defer windows.FreeLibrary(h)
	getPlatform, err := windows.GetProcAddress(h, "GetPlatform")
	if err != nil {
		r.clearMarker()
		r.fail("Platform.dll has no GetPlatform export")
		return
	}
	platform, _, _ := syscall.SyscallN(getPlatform)
	if platform == 0 {
		r.clearMarker()
		r.fail("GetPlatform returned null")
		return
	}
	cpu, detail := r.initPlatform(platform, nil, false)
	r.clearMarker()
	if cpu == 0 {
		r.fail(detail)
		return
	}
	defer vcall(platform, slotPlatformUnInit)

	var buf [ryzenParamsBuf]byte
	tick := time.NewTicker(1000 * time.Millisecond) // SDK guidance: at most once a second
	defer tick.Stop()
	for {
		for i := range buf {
			buf[i] = 0
		}
		rc := int32(vcall(cpu, slotCPUGetCPUParams, uintptr(unsafe.Pointer(&buf[0]))))
		if rc == 0 {
			rd, derr := decodeRyzenParams((*ryzenParams)(unsafe.Pointer(&buf[0])), time.Now())
			r.mu.Lock()
			if derr == nil {
				r.state, r.detail, r.last, r.have = RyzenRunning, "AMD Ryzen Master Monitoring SDK", rd, true
			} else {
				r.state, r.detail, r.have = RyzenUnavailable, "SDK returned unusable data: "+derr.Error(), false
			}
			r.mu.Unlock()
		} else {
			r.mu.Lock()
			r.state, r.detail, r.have = RyzenUnavailable, fmt.Sprintf("GetCPUParameters failed (%d)", rc), false
			r.mu.Unlock()
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

func (r *RyzenSDK) clearMarker() { _ = r.marker.set(false) }

// initPlatform runs Init and finds CPU device 0, validating that it really is
// the CPU object before any read is attempted.
func (r *RyzenSDK) initPlatform(platform uintptr, vendor *byte, cpuOnly bool) (cpu uintptr, why string) {
	// Init(pszBoardVendor, bUseCPUOnly); the thread's last error is the only
	// hint the SDK leaves when it fails (a driver handle it could not open).
	fn := peekPtr(peekPtr(platform) + slotPlatformInit*unsafe.Sizeof(uintptr(0)))
	flag := uintptr(0)
	if cpuOnly {
		flag = 1
	}
	ok, _, lastErr := syscall.SyscallN(fn, platform, uintptr(unsafe.Pointer(vendor)), flag)
	runtime.KeepAlive(vendor)
	if byte(ok) == 0 {
		why := "AMD SDK Init failed (unsupported CPU, or its driver could not start)"
		if lastErr != 0 {
			why += fmt.Sprintf("; Windows error %d: %v", uint32(lastErr), lastErr)
		}
		return 0, why
	}
	// From here on the SDK may have started threads and opened its driver:
	// any failure must UnInit before the caller unloads the DLL.
	fail := func(msg string) (uintptr, string) {
		vcall(platform, slotPlatformUnInit)
		return 0, msg
	}
	mgr := vcall(platform, slotPlatformDevMgr)
	if mgr == 0 {
		return fail("AMD SDK has no device manager")
	}
	dev := vcall(mgr, slotMgrGetDevice, dtCPU, 0)
	if dev == 0 {
		return fail("AMD SDK found no CPU device")
	}
	if t := int32(vcall(dev, slotDeviceGetType)); t != dtCPU {
		return fail(fmt.Sprintf("AMD SDK device 0 is not a CPU (type %d)", t))
	}
	name := wstr(vcall(dev, slotDeviceGetName))
	if name == "" {
		return fail("AMD SDK returned no CPU name")
	}
	pkg := wstr(vcall(dev, slotCPUGetPackage))
	r.mu.Lock()
	r.cpu, r.pkg = name, pkg
	r.mu.Unlock()
	return dev, ""
}

// ---- test seam ------------------------------------------------------------------

// pollFake runs the same init/validate/decode path against an in-memory object
// graph; used by tests that stand in for the native DLL.
func (r *RyzenSDK) pollFake(platform uintptr) (RyzenReading, error) {
	cpu, why := r.initPlatform(platform, nil, false)
	if cpu == 0 {
		return RyzenReading{}, errors.New(why)
	}
	var buf [ryzenParamsBuf]byte
	if rc := int32(vcall(cpu, slotCPUGetCPUParams, uintptr(unsafe.Pointer(&buf[0])))); rc != 0 {
		return RyzenReading{}, fmt.Errorf("GetCPUParameters rc=%d", rc)
	}
	return decodeRyzenParams((*ryzenParams)(unsafe.Pointer(&buf[0])), time.Now())
}
