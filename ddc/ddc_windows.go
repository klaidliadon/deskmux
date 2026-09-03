// Package ddc talks DDC/CI to attached monitors through the Windows monitor
// configuration API in dxva2.dll.
//
// This package is Windows-only and deliberately narrow: it exposes the VCP
// get/set and capabilities calls, nothing more.
//
// One caveat matters more than any API detail: DDC/CI writes are
// fire-and-forget. SetVCP returning nil means the bytes were sent, not that
// the monitor obeyed. Some panels accept writes to registers they then
// silently ignore. Use SetVCPVerified where the register can be read back.
package ddc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/klaidliadon/deskmux/vcp"
)

var (
	_user32 = syscall.NewLazyDLL("user32.dll")
	_dxva2  = syscall.NewLazyDLL("dxva2.dll")

	_enumDisplayMonitors = _user32.NewProc("EnumDisplayMonitors")

	_getNumPhysicalMonitors = _dxva2.NewProc("GetNumberOfPhysicalMonitorsFromHMONITOR")
	_getPhysicalMonitors    = _dxva2.NewProc("GetPhysicalMonitorsFromHMONITOR")
	_destroyPhysicalMonitor = _dxva2.NewProc("DestroyPhysicalMonitors")
	_getVCPFeature          = _dxva2.NewProc("GetVCPFeatureAndVCPFeatureReply")
	_setVCPFeature          = _dxva2.NewProc("SetVCPFeature")
	_getCapabilitiesLen     = _dxva2.NewProc("GetCapabilitiesStringLength")
	_getCapabilities        = _dxva2.NewProc("CapabilitiesRequestAndCapabilitiesReply")
)

// The I2C bus is shared and unarbitrated: any other process talking DDC can
// collide with us, and the transaction comes back as "error receiving data
// from the device on the I2C bus". Transient failures are normal.
//
// Kept deliberately gentle. Hammering a wedged DDC engine is what puts some
// panels (notably the LG 45GX950A) into a state where they stop answering
// entirely until power-cycled.
const (
	_retries = 4
	_backoff = 80 * time.Millisecond
)

// ErrNoMonitors is returned when Windows reports no physical monitors. On
// panels whose DDC engine has wedged this is what surfaces, even though the
// display itself is working.
var ErrNoMonitors = errors.New("no physical monitors found")

// physicalMonitor mirrors PHYSICAL_MONITOR: HANDLE + WCHAR[128] = 264 bytes.
type physicalMonitor struct {
	Handle      syscall.Handle
	Description [128]uint16
}

// Monitor is one physical display.
type Monitor struct {
	Handle syscall.Handle
	Name   string
}

// Set owns the native handles and must be closed.
type Set struct {
	Monitors []Monitor

	chunks [][]physicalMonitor
}

// Open enumerates every physical monitor Windows can see.
func Open() (*Set, error) {
	handles, err := monitorHandles()
	if err != nil {
		return nil, err
	}

	set := &Set{}
	for _, h := range handles {
		var count uint32
		if r, _, _ := _getNumPhysicalMonitors.Call(h, uintptr(unsafe.Pointer(&count))); r == 0 || count == 0 {
			continue
		}

		arr := make([]physicalMonitor, count)
		if r, _, _ := _getPhysicalMonitors.Call(h, uintptr(count), uintptr(unsafe.Pointer(&arr[0]))); r == 0 {
			continue
		}

		set.chunks = append(set.chunks, arr)
		for i := range arr {
			set.Monitors = append(set.Monitors, Monitor{
				Handle: arr[i].Handle,
				Name:   syscall.UTF16ToString(arr[i].Description[:]),
			})
		}
	}

	if len(set.Monitors) == 0 {
		return nil, ErrNoMonitors
	}
	return set, nil
}

// The enumeration callback is registered once, at package initialisation.
//
// syscall.NewCallback identifies callbacks by function value, so a closure
// built inside monitorHandles would register a new one on every call. Those
// registrations are never released and the runtime throws -- fatally, not
// recoverably -- past a fixed ceiling of a couple of thousand. A daemon
// retrying a monitor that is asleep or on another input reaches that in under
// a day, which is precisely when it is least welcome.
//
// EnumDisplayMonitors invokes the callback synchronously on the calling
// thread, so a mutex around the shared slice is sufficient.
var (
	_enumMu      sync.Mutex
	_enumHandles []uintptr

	_enumCallback = syscall.NewCallback(func(monitor, hdc, clip, data uintptr) uintptr {
		_enumHandles = append(_enumHandles, monitor)
		return 1 // continue enumeration
	})
)

func monitorHandles() ([]uintptr, error) {
	_enumMu.Lock()
	defer _enumMu.Unlock()

	_enumHandles = _enumHandles[:0]

	if r, _, err := _enumDisplayMonitors.Call(0, 0, _enumCallback, 0); r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors: %w", err)
	}
	return slices.Clone(_enumHandles), nil
}

// Close releases the native handles.
func (s *Set) Close() {
	for _, chunk := range s.chunks {
		if len(chunk) > 0 {
			_destroyPhysicalMonitor.Call(uintptr(len(chunk)), uintptr(unsafe.Pointer(&chunk[0])))
		}
	}
	s.chunks = nil
	s.Monitors = nil
}

// Reading is one reply from GetVCPFeatureAndVCPFeatureReply.
type Reading struct {
	Type    uint32 // 0 = momentary, 1 = set parameter
	Current vcp.Level
	Max     vcp.Level
}

func retry(op func() error) error {
	var err error
	for attempt := range _retries {
		if err = op(); err == nil {
			return nil
		}
		// No sleep after the final attempt: there is nothing left to wait
		// for, and every terminal failure was paying an extra backoff. Probe
		// reads eight registers and an unsupported one fails all four times.
		if attempt == _retries-1 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * _backoff)
	}
	return err
}

// GetVCP reads a VCP feature.
//
// The Windows API hands back DWORDs even though DDC/CI carries values as two
// bytes, so the reply is read into uint32s and narrowed to vcp.Level.
func (m Monitor) GetVCP(code vcp.Code) (Reading, error) {
	var kind, current, maximum uint32

	err := retry(func() error {
		r, _, callErr := _getVCPFeature.Call(
			uintptr(m.Handle),
			uintptr(code),
			uintptr(unsafe.Pointer(&kind)),
			uintptr(unsafe.Pointer(&current)),
			uintptr(unsafe.Pointer(&maximum)),
		)
		if r == 0 {
			return fmt.Errorf("GetVCPFeatureAndVCPFeatureReply(%s): %w", code, callErr)
		}
		return nil
	})
	if err != nil {
		return Reading{}, err
	}

	return Reading{
		Type:    kind,
		Current: vcp.Level(current),
		Max:     vcp.Level(maximum),
	}, nil
}

// SetVCP writes a VCP feature, retrying transient bus errors.
//
// A nil error means the bytes were sent, not that the monitor obeyed.
func (m Monitor) SetVCP(code vcp.Code, value vcp.Level) error {
	return retry(func() error { return m.SetVCPOnce(code, value) })
}

// SetVCPOnce writes without the retry ladder, for latency-sensitive callers
// such as volume keys where a failed write is better superseded by the next
// keystroke than retried for up to a second.
func (m Monitor) SetVCPOnce(code vcp.Code, value vcp.Level) error {
	r, _, err := _setVCPFeature.Call(uintptr(m.Handle), uintptr(code), uintptr(value))
	if r == 0 {
		return fmt.Errorf("SetVCPFeature(%s, %s): %w", code, value, err)
	}
	return nil
}

// SetVCPVerified writes, waits, reads back, and reports whether it stuck.
func (m Monitor) SetVCPVerified(code vcp.Code, value vcp.Level, settle time.Duration) (landed bool, readback vcp.Level, err error) {
	if err = m.SetVCP(code, value); err != nil {
		return false, 0, err
	}

	time.Sleep(settle)

	v, err := m.GetVCP(code)
	if err != nil {
		return false, 0, err
	}
	return v.Current == value, v.Current, nil
}

// Capabilities returns the raw DDC capabilities string.
func (m Monitor) Capabilities() (string, error) {
	var length uint32
	err := retry(func() error {
		r, _, callErr := _getCapabilitiesLen.Call(uintptr(m.Handle), uintptr(unsafe.Pointer(&length)))
		if r == 0 || length == 0 {
			return fmt.Errorf("GetCapabilitiesStringLength: %w", callErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	buf := make([]byte, length)
	err = retry(func() error {
		r, _, callErr := _getCapabilities.Call(
			uintptr(m.Handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(length))
		if r == 0 {
			return fmt.Errorf("CapabilitiesRequestAndCapabilitiesReply: %w", callErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf), "\x00"), nil
}
