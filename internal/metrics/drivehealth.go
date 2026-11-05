//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlStorageQueryProperty = 0x002D1400

	propStorageDevice     = 0  // StorageDeviceProperty
	propDeviceTemperature = 52 // StorageDeviceTemperatureProperty (12 is the power property)
	propProtocolSpecific  = 50 // StorageDeviceProtocolSpecificProperty
	protocolNVMe          = 3
	nvmeDataTypeLogPage   = 2
	nvmeLogPageHealthInfo = 2
	protoSpecificSize     = 40 // STORAGE_PROTOCOL_SPECIFIC_DATA (Windows 10 1903+)
	healthLogBytes        = 512
)

// DriveHealth describes one physical drive and what Windows will tell us about
// it without a vendor driver: model, bus, temperature and the NVMe health log.
type DriveHealth struct {
	Index       int     `json:"index"`
	Model       string  `json:"model"`
	Bus         string  `json:"bus"`
	TempC       float64 `json:"tempC"`
	HasTemp     bool    `json:"hasTemp"`
	HasSMART    bool    `json:"hasSmart"`
	Critical    uint8   `json:"criticalWarning"`
	SparePct    int     `json:"sparePct"`
	SpareThresh int     `json:"spareThresh"`
	UsedPct     int     `json:"usedPct"`
	ReadBytes   float64 `json:"readBytes"`
	WrittenByte float64 `json:"writtenBytes"`
	HostReads   float64 `json:"hostReads"`
	HostWrites  float64 `json:"hostWrites"`
	BusyMins    float64 `json:"busyMinutes"`
	PowerCycles float64 `json:"powerCycles"`
	PowerOnHrs  float64 `json:"powerOnHours"`
	Unsafe      float64 `json:"unsafeShutdowns"`
	MediaErrors float64 `json:"mediaErrors"`

	stalled bool // the drive did not answer within driveReadTimeout
}

// storagePropertyQuery is STORAGE_PROPERTY_QUERY followed by room for
// STORAGE_PROTOCOL_SPECIFIC_DATA (AdditionalParameters).
type storagePropertyQuery struct {
	PropertyID uint32
	QueryType  uint32
	Extra      [protoSpecificSize]byte
}

func openPhysical(i int) (windows.Handle, error) {
	name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\PhysicalDrive%d`, i))
	// access 0 is enough for IOCTL_STORAGE_QUERY_PROPERTY: no elevation needed
	return windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
}

func storageQuery(h windows.Handle, id uint32, extra []byte, outSize int) ([]byte, error) {
	var q storagePropertyQuery
	q.PropertyID = id
	inLen := uint32(12) // sizeof(STORAGE_PROPERTY_QUERY) incl. AdditionalParameters[1], padded
	if extra != nil {
		copy(q.Extra[:], extra)
		inLen = uint32(unsafe.Sizeof(q))
	}
	out := make([]byte, outSize)
	var ret uint32
	err := windows.DeviceIoControl(h, ioctlStorageQueryProperty,
		(*byte)(unsafe.Pointer(&q)), inLen, &out[0], uint32(len(out)), &ret, nil)
	if err != nil {
		return nil, err
	}
	return out[:ret], nil
}

// busName names a STORAGE_BUS_TYPE.
func busName(b uint32) string {
	switch b {
	case 1:
		return "SCSI"
	case 3, 11:
		return "SATA"
	case 7:
		return "USB"
	case 8:
		return "RAID" // Intel RST / VMD and other RAID controllers
	case 9:
		return "iSCSI"
	case 10:
		return "SAS"
	case 12:
		return "SD"
	case 13:
		return "MMC"
	case 14, 15:
		return "Virtual"
	case 16:
		return "Storage Spaces"
	case 17:
		return "NVMe"
	case 18:
		return "SCM"
	case 19:
		return "UFS"
	}
	return fmt.Sprintf("bus %d", b)
}

// ReadDriveHealth queries physical drives enumerated by Windows and returns
// what it could read. RAID controllers may expose only logical drives; member
// sensors hidden by their firmware cannot be queried through this interface.
func ReadDriveHealth() []DriveHealth { return readDriveInventory(true) }

const (
	// driveReadTimeout bounds one inventory: a drive whose IOCTL hangs (a
	// failing disk, a stuck RAID or USB bridge) is reported without readings
	// instead of holding back every other drive.
	driveReadTimeout = 4 * time.Second
	// driveDescriptorTTL: model and bus are static for a drive, but an index
	// can be reused after hot-plug, so cached descriptors expire.
	driveDescriptorTTL = 5 * time.Minute
)

type driveCacheEntry struct {
	desc DriveHealth // Index, Model, Bus only
	at   time.Time
	busy bool // a read of this drive is still blocked in the driver
}

var driveCache = struct {
	sync.Mutex
	m map[int]*driveCacheEntry
}{m: map[int]*driveCacheEntry{}}

// readDriveInventory reads every physical drive concurrently: full health
// (temperature, NVMe SMART), or with full false only the model and bus. A
// drive still blocked from an earlier read is not queried again until it
// returns; meanwhile it is listed with its cached descriptor and no readings.
func readDriveInventory(full bool) []DriveHealth {
	indices, err := discoverPhysicalDrives()
	if err != nil {
		return nil
	}
	return readDrivesWith(indices, full, driveReadTimeout, readOneDrive)
}

type driveResult struct {
	i  int
	d  DriveHealth
	ok bool
}

func readDrivesWith(indices []int, full bool, timeout time.Duration, read func(idx int, full bool, desc *DriveHealth) (DriveHealth, bool)) []DriveHealth {
	now := time.Now()
	type job struct {
		i, idx int
		desc   *DriveHealth
	}
	var jobs []job
	results := make([]DriveHealth, len(indices))
	state := make([]int8, len(indices)) // 0 pending, 1 done, -1 skip (could not open)
	driveCache.Lock()
	live := make(map[int]bool, len(indices))
	for i, idx := range indices {
		live[idx] = true
		e := driveCache.m[idx]
		if e == nil {
			e = &driveCacheEntry{}
			driveCache.m[idx] = e
		}
		if e.busy {
			results[i], state[i] = stalledDrive(e, idx), 1
			continue
		}
		e.busy = true
		var desc *DriveHealth
		if !e.at.IsZero() && now.Sub(e.at) < driveDescriptorTTL {
			d := e.desc
			desc = &d
		}
		jobs = append(jobs, job{i, idx, desc})
	}
	for idx, e := range driveCache.m {
		if !live[idx] && !e.busy {
			delete(driveCache.m, idx) // removed drive: forget its descriptor
		}
	}
	driveCache.Unlock()

	done := make(chan driveResult, len(jobs)) // buffered: a late reader never blocks
	for _, j := range jobs {
		go func(j job) {
			d, ok := read(j.idx, full, j.desc)
			driveCache.Lock()
			if e := driveCache.m[j.idx]; e != nil {
				e.busy = false
				// Only a descriptor the drive actually returned (it names a bus) is cached.
				if ok && d.Bus != "" && (j.desc == nil || d.Model != j.desc.Model || d.Bus != j.desc.Bus) {
					e.desc, e.at = DriveHealth{Index: j.idx, Model: d.Model, Bus: d.Bus}, time.Now()
				}
			}
			driveCache.Unlock()
			done <- driveResult{j.i, d, ok}
		}(j)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
wait:
	for range jobs {
		select {
		case r := <-done:
			if r.ok {
				results[r.i], state[r.i] = r.d, 1
			} else {
				state[r.i] = -1
			}
		case <-timer.C:
			break wait
		}
	}
	out := make([]DriveHealth, 0, len(indices))
	driveCache.Lock()
	for i, idx := range indices {
		switch state[i] {
		case 1:
			out = append(out, results[i])
		case 0: // still blocked in the driver
			e := driveCache.m[idx]
			if e == nil {
				e = &driveCacheEntry{}
			}
			out = append(out, stalledDrive(e, idx))
		}
	}
	driveCache.Unlock()
	return out
}

// stalledDrive lists a drive that is not answering, without readings.
func stalledDrive(e *driveCacheEntry, idx int) DriveHealth {
	d := e.desc
	d.Index = idx
	if d.Model == "" {
		d.Model = fmt.Sprintf("Physical drive %d", idx)
	}
	d.stalled = true
	return d
}

// readOneDrive opens and reads one drive; false when it cannot be opened.
func readOneDrive(idx int, full bool, desc *DriveHealth) (DriveHealth, bool) {
	h, err := openPhysical(idx)
	if err != nil {
		return DriveHealth{}, false
	}
	defer windows.CloseHandle(h)
	return readDriveHealthWith(idx, func(id uint32, extra []byte, outSize int) ([]byte, error) {
		return storageQuery(h, id, extra, outSize)
	}, desc, full), true
}

func readDriveHealth(idx int, query func(uint32, []byte, int) ([]byte, error)) DriveHealth {
	return readDriveHealthWith(idx, query, nil, true)
}

// readDriveHealthWith takes model and bus from desc when given (instead of
// querying the descriptor again) and stops after them unless full.
func readDriveHealthWith(idx int, query func(uint32, []byte, int) ([]byte, error), desc *DriveHealth, full bool) DriveHealth {
	d := DriveHealth{Index: idx}
	le := binary.LittleEndian

	// model + bus: STORAGE_DEVICE_DESCRIPTOR
	if desc != nil {
		d.Model, d.Bus = desc.Model, desc.Bus
	} else if b, err := query(propStorageDevice, nil, 1024); err == nil && len(b) >= 36 {
		str := func(off uint32) string {
			if off == 0 || int(off) >= len(b) {
				return ""
			}
			end := int(off)
			for end < len(b) && b[end] != 0 {
				end++
			}
			return strings.TrimSpace(string(b[off:end]))
		}
		vendor, product := str(le.Uint32(b[12:])), str(le.Uint32(b[16:]))
		d.Model = strings.TrimSpace(vendor + " " + product)
		d.Bus = busName(le.Uint32(b[28:]))
	}
	if d.Model == "" {
		d.Model = fmt.Sprintf("Physical drive %d", idx)
	}
	if !full {
		return d
	}

	// temperature: DEVICE_TEMPERATURE_DATA_DESCRIPTOR (works for NVMe and many SATA)
	// DEVICE_TEMPERATURE_DATA_DESCRIPTOR: a 24-byte header, then InfoCount
	// 16-byte DEVICE_TEMPERATURE_INFO entries whose Temperature is at +2.
	if b, err := query(propDeviceTemperature, nil, 256); err == nil && len(b) >= 24+16 {
		count := int(le.Uint16(b[12:]))
		if count > 0 && len(b) >= 24+16*count {
			t := int16(le.Uint16(b[26:])) // TemperatureInfo[0].Temperature, deg C
			if t > -50 && t < 150 {
				d.TempC, d.HasTemp = float64(t), true
			}
		}
	}

	// NVMe health/SMART log page
	if d.Bus == "NVMe" {
		var sp [protoSpecificSize]byte
		le.PutUint32(sp[0:], protocolNVMe)
		le.PutUint32(sp[4:], nvmeDataTypeLogPage)
		le.PutUint32(sp[8:], nvmeLogPageHealthInfo)
		le.PutUint32(sp[12:], 0)
		le.PutUint32(sp[16:], protoSpecificSize) // ProtocolDataOffset
		le.PutUint32(sp[20:], healthLogBytes)    // ProtocolDataLength
		b, err := query(propProtocolSpecific, sp[:], 8+protoSpecificSize+healthLogBytes)
		if err == nil && len(b) >= 8+protoSpecificSize {
			off := le.Uint32(b[8+16:]) // returned ProtocolDataOffset
			// The IOCTL may succeed while a bridge/driver returns an incomplete
			// descriptor. Validate the declared payload before trusting any health
			// bytes; otherwise the descriptor itself can become a healthy log.
			version, descriptorSize := le.Uint32(b), le.Uint32(b[4:])
			length := le.Uint32(b[28:])
			base := uint64(8) + uint64(off)
			end := base + uint64(length)
			if version >= 8+protoSpecificSize && descriptorSize >= version &&
				le.Uint32(b[8:]) == protocolNVMe && le.Uint32(b[12:]) == nvmeDataTypeLogPage &&
				off >= protoSpecificSize && base >= uint64(descriptorSize) &&
				length >= healthLogBytes && end <= uint64(len(b)) {
				log := b[int(base):int(end)]
				u128 := func(o int) float64 { // low 64 bits are plenty for these counters
					return float64(le.Uint64(log[o:]))
				}
				d.HasSMART = true
				d.Critical = log[0]
				if k := float64(le.Uint16(log[1:])) - 273.15; k > -50 && k < 150 {
					// same plausibility band as the temperature descriptor: a
					// not-reported 0xFFFF would otherwise read as 65261 °C
					d.TempC, d.HasTemp = k, true
				}
				d.SparePct = int(log[3])
				d.SpareThresh = int(log[4])
				d.UsedPct = int(log[5])
				d.ReadBytes = u128(32) * 512000
				d.WrittenByte = u128(48) * 512000
				d.HostReads = u128(64)
				d.HostWrites = u128(80)
				d.BusyMins = u128(96)
				d.PowerCycles = u128(112)
				d.PowerOnHrs = u128(128)
				d.Unsafe = u128(144)
				d.MediaErrors = u128(160)
			}
		}
	}
	d.TempC = math.Round(d.TempC*10) / 10
	return d
}
