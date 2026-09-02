package main

// Windows audio endpoint control, via the Core Audio COM API.
//
// Only used to pin the endpoint at 100%. Once the volume keys drive the
// monitor's own VCP 0x62, any attenuation here is a second, redundant stage
// that costs bit depth for nothing.
//
// Implementation note: IAudioEndpointVolume::SetMasterVolumeLevelScalar takes
// a float, and the Win64 ABI passes floats in XMM registers -- which Go's
// syscall.SyscallN cannot populate. So instead of setting the level directly
// we call VolumeStepUp (integer args only) until the level reads 1.0. Reading
// the level back is fine because it is an out-parameter in memory, not a
// register-passed float.

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioEndpointVolume = guid{0x5CDF2C82, 0x841E, 0x4546, [8]byte{0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}}
)

// Vtable slots.
const (
	slotRelease = 2

	// IMMDeviceEnumerator
	slotGetDefaultAudioEndpoint = 4
	// IMMDevice
	slotActivate          = 3
	slotOpenPropertyStore = 4
	slotGetId             = 5
	// IPropertyStore
	slotGetValue = 5
	// IAudioEndpointVolume
	slotGetMasterVolumeLevelScalar = 9
	slotSetMute                    = 14
	slotGetMute                    = 15
	slotVolumeStepUp               = 17
)

// comCall invokes vtable slot idx on a COM interface pointer.
func comCall(this uintptr, idx int, a ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(this))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	args := make([]uintptr, 0, len(a)+1)
	args = append(args, this)
	args = append(args, a...)
	r, _, _ := syscall.SyscallN(fn, args...)
	return r
}

// withEndpointVolume runs fn with an IAudioEndpointVolume for the default
// render device. COM is initialised and torn down around it on a locked
// thread, so callers need not care about apartment state.
func withEndpointVolume(fn func(vol uintptr) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// COINIT_APARTMENTTHREADED = 2
	hr, _, _ := procCoInitializeEx.Call(0, 2)
	// S_OK (0) and S_FALSE (1) both mean usable; RPC_E_CHANGED_MODE (0x80010106)
	// means someone already initialised this thread differently -- still usable.
	if int32(hr) < 0 && uint32(hr) != 0x80010106 {
		return fmt.Errorf("CoInitializeEx failed: 0x%08X", uint32(hr))
	}
	defer procCoUninitialize.Call()

	var enum uintptr
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		23, // CLSCTX_ALL
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)),
	)
	if int32(hr) < 0 {
		return fmt.Errorf("CoCreateInstance(MMDeviceEnumerator) failed: 0x%08X", uint32(hr))
	}
	defer comCall(enum, slotRelease)

	var dev uintptr
	// eRender = 0, eMultimedia = 1
	if hr := comCall(enum, slotGetDefaultAudioEndpoint, 0, 1, uintptr(unsafe.Pointer(&dev))); int32(hr) < 0 {
		return fmt.Errorf("GetDefaultAudioEndpoint failed: 0x%08X", uint32(hr))
	}
	defer comCall(dev, slotRelease)

	var vol uintptr
	if hr := comCall(dev, slotActivate,
		uintptr(unsafe.Pointer(&iidIAudioEndpointVolume)), 23, 0,
		uintptr(unsafe.Pointer(&vol))); int32(hr) < 0 {
		return fmt.Errorf("Activate(IAudioEndpointVolume) failed: 0x%08X", uint32(hr))
	}
	defer comCall(vol, slotRelease)

	return fn(vol)
}

// audioSession keeps one IMMDeviceEnumerator alive across polls.
//
// Creating the enumerator per poll (CoCreateInstance + teardown) cost ~12ms
// and showed up as a steady 1.2% of a core in an otherwise idle process.
// The caller must hold the OS thread and have COM initialised: COM apartments
// are per-thread, so the session cannot outlive or migrate off that thread.
type audioSession struct{ enum uintptr }

func newAudioSession() (*audioSession, error) {
	var enum uintptr
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, 23,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)))
	if int32(hr) < 0 {
		return nil, fmt.Errorf("CoCreateInstance failed: 0x%08X", uint32(hr))
	}
	return &audioSession{enum: enum}, nil
}

func (a *audioSession) close() {
	if a != nil && a.enum != 0 {
		comCall(a.enum, slotRelease)
		a.enum = 0
	}
}

// defaultDevice returns the default render endpoint; caller must release it.
func (a *audioSession) defaultDevice() (uintptr, error) {
	var dev uintptr
	if hr := comCall(a.enum, slotGetDefaultAudioEndpoint, 0, 1, uintptr(unsafe.Pointer(&dev))); int32(hr) < 0 {
		return 0, fmt.Errorf("GetDefaultAudioEndpoint failed: 0x%08X", uint32(hr))
	}
	return dev, nil
}

// deviceID is much cheaper than reading the friendly name, so the poll loop
// compares IDs and only falls back to the name when the device actually
// changes.
func deviceID(dev uintptr) (string, error) {
	var p uintptr
	if hr := comCall(dev, slotGetId, uintptr(unsafe.Pointer(&p))); int32(hr) < 0 {
		return "", fmt.Errorf("IMMDevice::GetId failed: 0x%08X", uint32(hr))
	}
	defer procCoTaskMemFree.Call(p)
	return utf16PtrToString(p), nil
}

func deviceName(dev uintptr) (string, error) {
	var store uintptr
	if hr := comCall(dev, slotOpenPropertyStore, 0, uintptr(unsafe.Pointer(&store))); int32(hr) < 0 {
		return "", fmt.Errorf("OpenPropertyStore failed: 0x%08X", uint32(hr))
	}
	defer comCall(store, slotRelease)

	var pv propVariant
	if hr := comCall(store, slotGetValue,
		uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&pv))); int32(hr) < 0 {
		return "", fmt.Errorf("GetValue failed: 0x%08X", uint32(hr))
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))

	if pv.Vt != vtLPWSTR || pv.Val == 0 {
		return "", fmt.Errorf("friendly name is not a string (vt=%d)", pv.Vt)
	}
	return utf16PtrToString(pv.Val), nil
}

// deviceVolume / pinDevice operate on an already-obtained IMMDevice.
func deviceVolume(dev uintptr) (float32, error) {
	var level float32
	err := withDeviceVolume(dev, func(vol uintptr) error {
		if hr := comCall(vol, slotGetMasterVolumeLevelScalar, uintptr(unsafe.Pointer(&level))); int32(hr) < 0 {
			return fmt.Errorf("GetMasterVolumeLevelScalar failed: 0x%08X", uint32(hr))
		}
		return nil
	})
	return level, err
}

func pinDevice(dev uintptr) (before float32, err error) {
	err = withDeviceVolume(dev, func(vol uintptr) error {
		read := func() (float32, error) {
			var v float32
			if hr := comCall(vol, slotGetMasterVolumeLevelScalar, uintptr(unsafe.Pointer(&v))); int32(hr) < 0 {
				return 0, fmt.Errorf("read failed: 0x%08X", uint32(hr))
			}
			return v, nil
		}
		var e error
		if before, e = read(); e != nil {
			return e
		}
		var muted int32
		if hr := comCall(vol, slotGetMute, uintptr(unsafe.Pointer(&muted))); int32(hr) >= 0 && muted != 0 {
			comCall(vol, slotSetMute, 0, 0)
		}
		for i := 0; i < 100; i++ {
			v, e := read()
			if e != nil {
				return e
			}
			if v >= 0.999 {
				return nil
			}
			if hr := comCall(vol, slotVolumeStepUp, 0); int32(hr) < 0 {
				return fmt.Errorf("VolumeStepUp failed: 0x%08X", uint32(hr))
			}
		}
		return fmt.Errorf("could not reach 100%%")
	})
	return before, err
}

func withDeviceVolume(dev uintptr, fn func(vol uintptr) error) error {
	var vol uintptr
	if hr := comCall(dev, slotActivate,
		uintptr(unsafe.Pointer(&iidIAudioEndpointVolume)), 23, 0,
		uintptr(unsafe.Pointer(&vol))); int32(hr) < 0 {
		return fmt.Errorf("Activate failed: 0x%08X", uint32(hr))
	}
	defer comCall(vol, slotRelease)
	return fn(vol)
}

type propertyKey struct {
	Fmtid guid
	Pid   uint32
}

// PROPVARIANT is 24 bytes on x64; we only ever read the VT_LPWSTR case.
type propVariant struct {
	Vt         uint16
	wReserved1 uint16
	wReserved2 uint16
	wReserved3 uint16
	Val        uintptr
	_          uintptr
}

const vtLPWSTR = 31

var (
	pkeyDeviceFriendlyName = propertyKey{
		Fmtid: guid{0xA45C254E, 0xDF1C, 0x4EFD, [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		Pid:   14,
	}
	procPropVariantClear = ole32.NewProc("PropVariantClear")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

// DefaultRenderName returns the friendly name of the current default playback
// device, e.g. "LG ULTRAGEAR+ (NVIDIA High Definition Audio)".
func DefaultRenderName() (string, error) {
	var name string
	err := withDefaultDevice(func(dev uintptr) error {
		var store uintptr
		// STGM_READ = 0
		if hr := comCall(dev, slotOpenPropertyStore, 0, uintptr(unsafe.Pointer(&store))); int32(hr) < 0 {
			return fmt.Errorf("OpenPropertyStore failed: 0x%08X", uint32(hr))
		}
		defer comCall(store, slotRelease)

		var pv propVariant
		if hr := comCall(store, slotGetValue,
			uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
			uintptr(unsafe.Pointer(&pv))); int32(hr) < 0 {
			return fmt.Errorf("IPropertyStore::GetValue failed: 0x%08X", uint32(hr))
		}
		defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))

		if pv.Vt != vtLPWSTR || pv.Val == 0 {
			return fmt.Errorf("friendly name property is not a string (vt=%d)", pv.Vt)
		}
		name = utf16PtrToString(pv.Val)
		return nil
	})
	return name, err
}

func utf16PtrToString(p uintptr) string {
	var out []uint16
	for i := 0; ; i++ {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i)*2))
		if c == 0 {
			break
		}
		out = append(out, c)
		if i > 4096 {
			break
		}
	}
	return syscall.UTF16ToString(out)
}

// withDefaultDevice is withEndpointVolume's sibling: it hands over the
// IMMDevice rather than its volume interface.
func withDefaultDevice(fn func(dev uintptr) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, 2)
	if int32(hr) < 0 && uint32(hr) != 0x80010106 {
		return fmt.Errorf("CoInitializeEx failed: 0x%08X", uint32(hr))
	}
	defer procCoUninitialize.Call()

	var enum uintptr
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, 23,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)))
	if int32(hr) < 0 {
		return fmt.Errorf("CoCreateInstance failed: 0x%08X", uint32(hr))
	}
	defer comCall(enum, slotRelease)

	var dev uintptr
	if hr := comCall(enum, slotGetDefaultAudioEndpoint, 0, 1, uintptr(unsafe.Pointer(&dev))); int32(hr) < 0 {
		return fmt.Errorf("GetDefaultAudioEndpoint failed: 0x%08X", uint32(hr))
	}
	defer comCall(dev, slotRelease)

	return fn(dev)
}

// WindowsVolume returns the default endpoint's master level, 0.0 to 1.0.
func WindowsVolume() (float32, error) {
	var level float32
	err := withEndpointVolume(func(vol uintptr) error {
		if hr := comCall(vol, slotGetMasterVolumeLevelScalar, uintptr(unsafe.Pointer(&level))); int32(hr) < 0 {
			return fmt.Errorf("GetMasterVolumeLevelScalar failed: 0x%08X", uint32(hr))
		}
		return nil
	})
	return level, err
}

// PinWindowsVolume drives the default endpoint to 100% and unmutes it.
// Returns the level it started at so callers can report a change.
func PinWindowsVolume() (before float32, err error) {
	err = withEndpointVolume(func(vol uintptr) error {
		read := func() (float32, error) {
			var v float32
			if hr := comCall(vol, slotGetMasterVolumeLevelScalar, uintptr(unsafe.Pointer(&v))); int32(hr) < 0 {
				return 0, fmt.Errorf("GetMasterVolumeLevelScalar failed: 0x%08X", uint32(hr))
			}
			return v, nil
		}

		var e error
		if before, e = read(); e != nil {
			return e
		}

		// Unmute; a muted endpoint at 100% is still silent.
		var muted int32
		if hr := comCall(vol, slotGetMute, uintptr(unsafe.Pointer(&muted))); int32(hr) >= 0 && muted != 0 {
			comCall(vol, slotSetMute, 0, 0)
		}

		// Step to the top. Endpoints expose ~50 steps; 100 is a safe bound.
		for i := 0; i < 100; i++ {
			v, e := read()
			if e != nil {
				return e
			}
			if v >= 0.999 {
				return nil
			}
			if hr := comCall(vol, slotVolumeStepUp, 0); int32(hr) < 0 {
				return fmt.Errorf("VolumeStepUp failed: 0x%08X", uint32(hr))
			}
		}
		return fmt.Errorf("could not reach 100%% after 100 steps")
	})
	return before, err
}
