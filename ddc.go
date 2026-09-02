package main

// Standard DDC/CI backend, via the Windows monitor configuration API in
// dxva2.dll. Everything reachable this way is verified working on the
// 45GX950A: brightness (0x10), volume (0x62), capabilities, and all reads.
//
// The one thing this API cannot do is set the DDC source address -- it
// hardcodes 0x51. See nvapi.go for the sidechannel that can.

import (
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The I2C bus is shared and unarbitrated: any other process talking DDC
// (Twinkle Tray polls continuously, for instance) can collide with us and
// the transaction comes back as "error receiving data from the device on the
// I2C bus". Transient failures are normal and retrying is the standard
// remedy -- ddcutil does the same thing.
// Kept deliberately gentle: hammering a wedged DDC engine is what put the
// 45GX950A into ERROR_GRAPHICS_I2C_ERROR_RECEIVING_DATA in the first place.
var (
	ddcRetries = 4
	ddcBackoff = 80 * time.Millisecond
)

func retryDDC(op func() error) error {
	var err error
	for i := 0; i < ddcRetries; i++ {
		if err = op(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * ddcBackoff)
	}
	return err
}

var (
	user32 = syscall.NewLazyDLL("user32.dll")
	dxva2  = syscall.NewLazyDLL("dxva2.dll")

	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")

	procGetNumPhysMon  = dxva2.NewProc("GetNumberOfPhysicalMonitorsFromHMONITOR")
	procGetPhysMon     = dxva2.NewProc("GetPhysicalMonitorsFromHMONITOR")
	procDestroyPhysMon = dxva2.NewProc("DestroyPhysicalMonitors")
	procGetVCP         = dxva2.NewProc("GetVCPFeatureAndVCPFeatureReply")
	procSetVCP         = dxva2.NewProc("SetVCPFeature")
	procGetCapsLen     = dxva2.NewProc("GetCapabilitiesStringLength")
	procGetCaps        = dxva2.NewProc("CapabilitiesRequestAndCapabilitiesReply")
)

// physicalMonitor mirrors PHYSICAL_MONITOR: HANDLE + WCHAR[128] = 264 bytes.
type physicalMonitor struct {
	Handle      syscall.Handle
	Description [128]uint16
}

// Monitor is one physical display we can talk DDC to.
type Monitor struct {
	Handle syscall.Handle
	Name   string
}

// Monitors owns the native handles and must be Closed.
type Monitors struct {
	List   []Monitor
	chunks [][]physicalMonitor
}

func (m *Monitors) Close() {
	for _, c := range m.chunks {
		if len(c) > 0 {
			procDestroyPhysMon.Call(uintptr(len(c)), uintptr(unsafe.Pointer(&c[0])))
		}
	}
	m.chunks = nil
	m.List = nil
}

// OpenMonitors enumerates every physical monitor Windows can see.
func OpenMonitors() (*Monitors, error) {
	var hmons []uintptr
	cb := syscall.NewCallback(func(hMonitor, hdc, lprc, data uintptr) uintptr {
		hmons = append(hmons, hMonitor)
		return 1 // continue enumeration
	})
	r, _, err := procEnumDisplayMonitors.Call(0, 0, cb, 0)
	if r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors failed: %v", err)
	}

	out := &Monitors{}
	for _, h := range hmons {
		var n uint32
		r, _, _ := procGetNumPhysMon.Call(h, uintptr(unsafe.Pointer(&n)))
		if r == 0 || n == 0 {
			continue
		}
		arr := make([]physicalMonitor, n)
		r, _, _ = procGetPhysMon.Call(h, uintptr(n), uintptr(unsafe.Pointer(&arr[0])))
		if r == 0 {
			continue
		}
		out.chunks = append(out.chunks, arr)
		for i := range arr {
			out.List = append(out.List, Monitor{
				Handle: arr[i].Handle,
				Name:   strings.TrimRight(syscall.UTF16ToString(arr[i].Description[:]), "\x00"),
			})
		}
	}
	if len(out.List) == 0 {
		return nil, fmt.Errorf("no physical monitors found")
	}
	return out, nil
}

// VCPValue is one reply from GetVCPFeatureAndVCPFeatureReply.
type VCPValue struct {
	Type    uint32 // 0 = momentary, 1 = set parameter
	Current uint32
	Max     uint32
}

// GetVCP reads a VCP feature. Reads use the standard 0x51 path and are
// reliable on this hardware.
func (m Monitor) GetVCP(code byte) (VCPValue, error) {
	var v VCPValue
	err := retryDDC(func() error {
		r, _, e := procGetVCP.Call(
			uintptr(m.Handle),
			uintptr(code),
			uintptr(unsafe.Pointer(&v.Type)),
			uintptr(unsafe.Pointer(&v.Current)),
			uintptr(unsafe.Pointer(&v.Max)),
		)
		if r == 0 {
			return fmt.Errorf("GetVCPFeatureAndVCPFeatureReply(0x%02X) failed: %v", code, e)
		}
		return nil
	})
	return v, err
}

// SetVCP writes a VCP feature.
//
// Note: DDC/CI writes are fire-and-forget. A nil error means "the bytes were
// sent", NOT "the monitor obeyed" -- the 45GX950A returns success for writes
// to 0x60 that it silently discards. Always confirm with a read where the
// register supports it.
func (m Monitor) SetVCP(code byte, value uint32) error {
	return retryDDC(func() error {
		r, _, e := procSetVCP.Call(uintptr(m.Handle), uintptr(code), uintptr(value))
		if r == 0 {
			return fmt.Errorf("SetVCPFeature(0x%02X, %d) failed: %v", code, value, e)
		}
		return nil
	})
}

// SetVCPOnce writes without the retry ladder. Used on latency-sensitive paths
// such as volume keys, where a failed write is better superseded by the next
// keystroke than retried for up to a second.
func (m Monitor) SetVCPOnce(code byte, value uint32) error {
	r, _, err := procSetVCP.Call(uintptr(m.Handle), uintptr(code), uintptr(value))
	if r == 0 {
		return fmt.Errorf("SetVCPFeature(0x%02X, %d) failed: %v", code, value, err)
	}
	return nil
}

// SetVCPVerified writes, waits, reads back, and reports whether it stuck.
func (m Monitor) SetVCPVerified(code byte, value uint32) (landed bool, readback uint32, err error) {
	if err = m.SetVCP(code, value); err != nil {
		return false, 0, err
	}
	sleepSettle()
	v, err := m.GetVCP(code)
	if err != nil {
		return false, 0, err
	}
	return v.Current == value, v.Current, nil
}

// Capabilities returns the raw DDC capabilities string.
func (m Monitor) Capabilities() (string, error) {
	var n uint32
	if err := retryDDC(func() error {
		r, _, e := procGetCapsLen.Call(uintptr(m.Handle), uintptr(unsafe.Pointer(&n)))
		if r == 0 || n == 0 {
			return fmt.Errorf("GetCapabilitiesStringLength failed: %v", e)
		}
		return nil
	}); err != nil {
		return "", err
	}

	buf := make([]byte, n)
	if err := retryDDC(func() error {
		r, _, e := procGetCaps.Call(uintptr(m.Handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(n))
		if r == 0 {
			return fmt.Errorf("CapabilitiesRequestAndCapabilitiesReply failed: %v", e)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf), "\x00"), nil
}
