//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"testing"
)

func TestDriveTemperatureDescriptorZeroAndSentinels(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  uint16
		want bool
	}{
		{"zero Celsius is real", 0, true},
		{"unavailable", 0x8000, false},
		{"invalid high", 0x7fff, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := readDriveHealth(0, func(id uint32, extra []byte, size int) ([]byte, error) {
				if id != propDeviceTemperature {
					return nil, fmt.Errorf("unsupported")
				}
				b := make([]byte, 40)
				binary.LittleEndian.PutUint16(b[12:], 1)
				binary.LittleEndian.PutUint16(b[26:], tt.raw)
				return b, nil
			})
			if d.HasTemp != tt.want || d.TempC != 0 {
				t.Fatalf("temperature = %v, %v; want availability %v and zero", d.TempC, d.HasTemp, tt.want)
			}
		})
	}
}

func TestNVMeTemperatureRejectsUnreported(t *testing.T) {
	for _, raw := range []uint16{0, 0xffff} {
		d := readDriveHealth(0, func(id uint32, extra []byte, size int) ([]byte, error) {
			switch id {
			case propStorageDevice:
				b := make([]byte, 36)
				binary.LittleEndian.PutUint32(b[28:], 17)
				return b, nil
			case propProtocolSpecific:
				b := validNVMeHealthResponse()
				binary.LittleEndian.PutUint32(b[24:], 40)
				binary.LittleEndian.PutUint16(b[49:], raw)
				return b, nil
			}
			return nil, fmt.Errorf("unsupported")
		})
		if d.HasTemp {
			t.Fatalf("NVMe raw %d reported temperature %v", raw, d.TempC)
		}
		if !d.HasSMART {
			t.Fatal("valid health log was discarded")
		}
	}
}

// Reads this PC's drives; any temperature reported must be a real one (the old
// code asked for the wrong property and read the wrong offset, giving none).
func TestDriveTemperaturesArePlausible(t *testing.T) {
	for _, d := range ReadDriveHealth() {
		t.Logf("drive %d: %s [%s] temp=%v %.0f°C smart=%v", d.Index, d.Model, d.Bus, d.HasTemp, d.TempC, d.HasSMART)
		if d.HasTemp && (d.TempC <= -50 || d.TempC >= 150) {
			t.Errorf("drive %d temperature %.1f °C is not plausible", d.Index, d.TempC)
		}
	}
}

func validNVMeHealthResponse() []byte {
	b := make([]byte, 8+protoSpecificSize+healthLogBytes)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 8+protoSpecificSize)
	le.PutUint32(b[4:], 8+protoSpecificSize)
	le.PutUint32(b[8:], protocolNVMe)
	le.PutUint32(b[12:], nvmeDataTypeLogPage)
	le.PutUint32(b[16:], nvmeLogPageHealthInfo)
	le.PutUint32(b[24:], protoSpecificSize)
	le.PutUint32(b[28:], healthLogBytes)
	return b
}

func TestNVMeRejectsMalformedSuccessfulResponses(t *testing.T) {
	for _, tt := range []struct {
		name     string
		offset   int
		value    uint32
		truncate int
	}{
		{"zero descriptor version", 0, 0, 0},
		{"short descriptor size", 4, 12, 0},
		{"wrong protocol", 8, 1, 0},
		{"wrong data type", 12, 1, 0},
		{"overlapping payload", 24, 0, 0},
		{"offset overflow", 24, 0xffffffff, 0},
		{"short reported payload", 28, 64, 0},
		{"oversized reported payload", 28, 0xffffffff, 0},
		{"truncated payload", -1, 0, 300},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := validNVMeHealthResponse()
			if tt.offset >= 0 {
				binary.LittleEndian.PutUint32(response[tt.offset:], tt.value)
			}
			if tt.truncate > 0 {
				response = response[:tt.truncate]
			}
			d := readDriveHealth(0, func(id uint32, extra []byte, size int) ([]byte, error) {
				switch id {
				case propStorageDevice:
					b := make([]byte, 36)
					binary.LittleEndian.PutUint32(b[28:], 17)
					return b, nil
				case propProtocolSpecific:
					return response, nil
				}
				return nil, fmt.Errorf("unsupported")
			})
			if d.HasSMART {
				t.Fatalf("malformed response reported valid SMART: %+v", d)
			}
		})
	}
}
