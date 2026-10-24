//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
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

// ReadDriveHealth queries \\.\PhysicalDrive0.. and returns what it could read.
func ReadDriveHealth() []DriveHealth {
	var out []DriveHealth
	misses := 0
	for i := 0; i < 32 && misses < 4; i++ {
		h, err := openPhysical(i)
		if err != nil {
			misses++
			continue
		}
		misses = 0
		out = append(out, readOneDrive(h, i))
		windows.CloseHandle(h)
	}
	return out
}

func readOneDrive(h windows.Handle, idx int) DriveHealth {
	d := DriveHealth{Index: idx}
	le := binary.LittleEndian

	// model + bus: STORAGE_DEVICE_DESCRIPTOR
	if b, err := storageQuery(h, propStorageDevice, nil, 1024); err == nil && len(b) >= 36 {
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

	// temperature: DEVICE_TEMPERATURE_DATA_DESCRIPTOR (works for NVMe and many SATA)
	// DEVICE_TEMPERATURE_DATA_DESCRIPTOR: a 24-byte header, then InfoCount
	// 16-byte DEVICE_TEMPERATURE_INFO entries whose Temperature is at +2.
	if b, err := storageQuery(h, propDeviceTemperature, nil, 256); err == nil && len(b) >= 24+16 {
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
		b, err := storageQuery(h, propProtocolSpecific, sp[:], 8+protoSpecificSize+healthLogBytes)
		if err == nil && len(b) >= 8+protoSpecificSize+64 {
			off := le.Uint32(b[8+16:]) // returned ProtocolDataOffset
			base := 8 + int(off)
			if base+200 <= len(b) {
				log := b[base:]
				u128 := func(o int) float64 { // low 64 bits are plenty for these counters
					return float64(le.Uint64(log[o:]))
				}
				d.HasSMART = true
				d.Critical = log[0]
				if k := le.Uint16(log[1:]); k > 0 {
					d.TempC, d.HasTemp = float64(k)-273.15, true
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
