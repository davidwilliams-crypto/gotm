//go:build windows

package metrics

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// gpuIdentity keeps discovery evidence separate from the public sampling model.
// Locations here are obtained from the device itself, never a vendor/model guess.
type gpuIdentity struct {
	gpuStatic
	pnp      string
	excluded bool
}

func enumerateGPUs() []gpuStatic {
	var dx []gpuIdentity
	for _, st := range enumerateDXGIGPUs() {
		// The legacy DXGI reader can guess these from matching model/name registry
		// entries. Discard that guess; SetupAPI supplies the exact devnode's driver.
		st.driver, st.date, st.location, st.bus = "", "", "", -1
		var hi, lo uint32
		if _, err := fmt.Sscanf(st.id, "0X%08X_0X%08X", &hi, &lo); err == nil {
			if b, d, f, ok := adapterAddress(lo, int32(hi)); ok {
				st.bus = int(b)
				st.location = fmt.Sprintf("%02X:%02X.%X", b, d, f)
			}
		}
		dx = append(dx, gpuIdentity{gpuStatic: st})
	}
	return mergeGPUDiscovery(dx, enumerateKernelGPUs(), enumeratePresentDisplayGPUs())
}

func softwareGPU(g gpuIdentity) bool {
	// Some old DXGI implementations fail to set DXGI_ADAPTER_FLAG_SOFTWARE.
	return g.software || (g.vendorID == 0x1414 && g.deviceID == 0x008c) || strings.EqualFold(g.name, "Microsoft Basic Render Driver")
}

func mergeGPUDiscovery(dx, kernel, present []gpuIdentity) []gpuStatic {
	var merged []gpuIdentity
	for _, source := range [][]gpuIdentity{dx, kernel, present} {
		counts := map[string]int{}
		for _, g := range source {
			if g.location != "" {
				counts[g.location]++
			}
		}
		for _, g := range source {
			match := -1
			for i, old := range merged {
				if (g.id != "" && g.id == old.id) || (g.pnp != "" && strings.EqualFold(g.pnp, old.pnp)) {
					match = i
					break
				}
			}
			if match < 0 && g.location != "" && counts[g.location] == 1 {
				for i, old := range merged {
					// Different known LUIDs are different logical adapters, even when backed
					// by the same PCI function (for example a linked/virtual adapter).
					if old.location == g.location && (g.id == "" || old.id == "" || g.id == old.id) {
						if match >= 0 {
							match = -1
							break
						}
						match = i
					}
				}
			}
			if match < 0 {
				merged = append(merged, g)
				continue
			}
			old := &merged[match]
			old.excluded = old.excluded || g.excluded
			old.software = old.software || g.software
			if g.id != "" {
				old.id = g.id
			}
			if g.pnp != "" {
				old.pnp = g.pnp
			}
			if g.name != "" {
				old.name = g.name
			}
			if g.vendorID != 0 {
				old.vendorID = g.vendorID
				old.deviceID = g.deviceID
			}
			if g.driver != "" {
				old.driver = g.driver
				old.date = g.date
			}
			if g.location != "" {
				old.location = g.location
				old.bus = g.bus
			}
			if old.ded == 0 {
				old.ded = g.ded
			}
			if old.shr == 0 {
				old.shr = g.shr
			}
		}
	}
	var out []gpuStatic
	for _, g := range merged {
		if g.excluded || softwareGPU(g) {
			continue
		}
		if g.id == "" && g.pnp != "" {
			g.id = "PNP:" + strings.ToUpper(g.pnp)
		}
		if g.id == "" {
			continue
		}
		if g.name == "" {
			g.name = "GPU " + g.id
		}
		out = append(out, g.gpuStatic)
	}
	return out
}

var procD3DKMTEnumAdapters2 = gdi32GPU.NewProc("D3DKMTEnumAdapters2")

// D3DKMT_ADAPTERINFO uses a 32-bit handle and 4-byte-aligned LUID (20 bytes).
type kmtAdapterInfo struct {
	Handle, Low      uint32
	High             int32
	Sources, Precise uint32
}
type kmtEnumAdapters struct {
	Count    uint32
	Adapters *kmtAdapterInfo
}

func kmtQuery(handle uint32, kind int32, data unsafe.Pointer, size uintptr) bool {
	q := struct {
		Handle uint32
		Kind   int32
		Data   unsafe.Pointer
		Size   uint32
	}{handle, kind, data, uint32(size)}
	r, _, _ := procD3DKMTQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q)))
	return int32(r) >= 0
}

func enumerateKernelGPUs() []gpuIdentity {
	for _, p := range []*windows.LazyProc{procD3DKMTEnumAdapters2, procD3DKMTQueryAdapterInfo, procD3DKMTCloseAdapter} {
		if p.Find() != nil {
			return nil
		}
	}
	// A bounded buffer also handles adapter arrival between the count and fill
	// calls without retaining handles from an earlier inventory.
	adapters := make([]kmtAdapterInfo, 64)
	arg := kmtEnumAdapters{Count: uint32(len(adapters)), Adapters: &adapters[0]}
	r, _, _ := procD3DKMTEnumAdapters2.Call(uintptr(unsafe.Pointer(&arg)))
	if int32(r) < 0 || arg.Count > uint32(len(adapters)) {
		return nil
	}
	var out []gpuIdentity
	for _, a := range adapters[:arg.Count] {
		if a.Handle == 0 {
			continue
		}
		g := gpuIdentity{gpuStatic: gpuStatic{id: fmt.Sprintf("0X%08X_0X%08X", uint32(a.High), a.Low), bus: -1}}
		var typ uint32
		if kmtQuery(a.Handle, 15, unsafe.Pointer(&typ), unsafe.Sizeof(typ)) {
			g.software, g.excluded = classifyKernelGPU(typ)

		} else if a.Sources == 0 {
			// Without a type query, only DXGI can establish a render-only GPU.
			procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&a.Handle)))
			continue
		}
		var reg [4][260]uint16
		if kmtQuery(a.Handle, 8, unsafe.Pointer(&reg), unsafe.Sizeof(reg)) {
			g.name = strings.TrimSpace(windows.UTF16ToString(reg[0][:]))
		}
		var addr struct{ Bus, Device, Function uint32 }
		if kmtQuery(a.Handle, 6, unsafe.Pointer(&addr), unsafe.Sizeof(addr)) {
			g.bus = int(addr.Bus)
			g.location = fmt.Sprintf("%02X:%02X.%X", addr.Bus, addr.Device, addr.Function)
		}
		var segments struct{ DedicatedVideo, DedicatedSystem, SharedSystem uint64 }
		if kmtQuery(a.Handle, 3, unsafe.Pointer(&segments), unsafe.Sizeof(segments)) {
			g.ded = segments.DedicatedVideo
			g.shr = segments.SharedSystem
		}
		procD3DKMTCloseAdapter.Call(uintptr(unsafe.Pointer(&a.Handle)))
		out = append(out, g)
	}
	return out
}

func pciIDPart(s, part string) uint32 {
	s = strings.ToUpper(s)
	i := strings.Index(s, part+"_")
	if i < 0 || len(s) < i+len(part)+5 {
		return 0
	}
	n, _ := strconv.ParseUint(s[i+len(part)+1:i+len(part)+5], 16, 32)
	return uint32(n)
}

func enumeratePresentDisplayGPUs() []gpuIdentity {
	display := windows.GUID{Data1: 0x4d36e968, Data2: 0xe325, Data3: 0x11ce, Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18}}
	set, err := windows.SetupDiGetClassDevsEx(&display, "", 0, windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil
	}
	defer set.Close()
	var out []gpuIdentity
	for i := 0; ; i++ {
		dev, err := set.EnumDeviceInfo(i)
		if err != nil {
			break
		}
		pnp, err := windows.SetupDiGetDeviceInstanceId(set, dev)
		if err != nil || pnp == "" {
			continue
		}
		g := gpuIdentity{gpuStatic: gpuStatic{bus: -1}, pnp: strings.ToUpper(pnp)}
		prop := func(p windows.SPDRP) interface{} { v, _ := set.DeviceRegistryProperty(dev, p); return v }
		g.name, _ = prop(windows.SPDRP_FRIENDLYNAME).(string)
		if g.name == "" {
			g.name, _ = prop(windows.SPDRP_DEVICEDESC).(string)
		}
		g.vendorID, g.deviceID = pciIDPart(pnp, "VEN"), pciIDPart(pnp, "DEV")
		// Bus/address have PCI semantics only for PCI devnodes.
		if strings.HasPrefix(g.pnp, `PCI\`) {
			b, bok := prop(windows.SPDRP_BUSNUMBER).(uint32)
			a, aok := prop(windows.SPDRP_ADDRESS).(uint32)
			if bok && aok && a != 0xffffffff {
				g.bus = int(b)
				g.location = fmt.Sprintf("%02X:%02X.%X", b, a>>16, a&0xffff)
			}
		}
		h, err := set.OpenDevRegKey(dev, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DRV, windows.KEY_QUERY_VALUE)
		if err == nil {
			k := registry.Key(h)
			g.driver, _, _ = k.GetStringValue("DriverVersion")
			g.date, _, _ = k.GetStringValue("DriverDate")
			if n, _, e := k.GetIntegerValue("HardwareInformation.qwMemorySize"); e == nil {
				g.ded = n
			} else if n, _, e := k.GetIntegerValue("HardwareInformation.MemorySize"); e == nil {
				g.ded = n
			} else if data, _, e := k.GetBinaryValue("HardwareInformation.MemorySize"); e == nil && len(data) == 4 {
				g.ded = uint64(binary.LittleEndian.Uint32(data))
			}
			if n, _, e := k.GetIntegerValue("AdapterLuid"); e == nil && n != 0 {
				g.id = fmt.Sprintf("0X%08X_0X%08X", uint32(n>>32), uint32(n))
			} else if data, _, e := k.GetBinaryValue("AdapterLuid"); e == nil && len(data) == 8 {
				g.id = fmt.Sprintf("0X%08X_0X%08X", binary.LittleEndian.Uint32(data[4:]), binary.LittleEndian.Uint32(data))
			}
			k.Close()
		}
		out = append(out, g)
	}
	return out
}

// Software display transports are legitimate display adapters. WARP has no
// display support; ComputeOnly describes MCDM devices, not graphics engines.
func classifyKernelGPU(flags uint32) (software, excluded bool) {
	return flags&(1<<2) != 0 && flags&((1<<1)|(1<<6)|(1<<7)) == 0, flags&(1<<11) != 0
}
