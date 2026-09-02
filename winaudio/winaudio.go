// Package winaudio controls Windows audio endpoints through the Core Audio
// COM API.
//
// It exists to answer two questions for a monitor-control tool: which device
// is currently playing, and can its volume be pinned so the monitor becomes
// the only attenuation stage.
//
// COM apartments are per-thread. Every call here must happen on a thread the
// caller has locked with runtime.LockOSThread and initialised with InitCOM;
// a Session must not outlive or migrate off that thread.
package winaudio

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	_ole32              = syscall.NewLazyDLL("ole32.dll")
	_coInitializeEx     = _ole32.NewProc("CoInitializeEx")
	_coUninitialize     = _ole32.NewProc("CoUninitialize")
	_coCreateInstance   = _ole32.NewProc("CoCreateInstance")
	_coTaskMemFree      = _ole32.NewProc("CoTaskMemFree")
	_propVariantClearFn = _ole32.NewProc("PropVariantClear")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	_clsidMMDeviceEnumerator = guid{0xBCDE0395, 0xE52F, 0x467C, [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	_iidIMMDeviceEnumerator  = guid{0xA95664D2, 0x9614, 0x4F35, [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	_iidIAudioEndpointVolume = guid{0x5CDF2C82, 0x841E, 0x4546, [8]byte{0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}}

	_pkeyDeviceFriendlyName = propertyKey{
		Fmtid: guid{0xA45C254E, 0xDF1C, 0x4EFD, [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
		Pid:   14,
	}
)

// Vtable slots.
const (
	_slotRelease = 2

	// IMMDeviceEnumerator
	_slotGetDefaultAudioEndpoint = 4

	// IMMDevice
	_slotActivate          = 3
	_slotOpenPropertyStore = 4
	_slotGetID             = 5

	// IPropertyStore
	_slotGetValue = 5

	// IAudioEndpointVolume
	_slotGetMasterVolumeScalar = 9
	_slotSetMute               = 14
	_slotGetMute               = 15
	_slotVolumeStepUp          = 17
)

const (
	_clsctxAll     = 23
	_apartmentMode = 2
	_eRender       = 0
	_eMultimedia   = 1
	_vtLPWSTR      = 31

	// RPC_E_CHANGED_MODE: the thread is already in a different apartment,
	// which is still usable for our purposes.
	_rpcChangedMode = 0x80010106
)

type propertyKey struct {
	Fmtid guid
	Pid   uint32
}

// propVariant is 24 bytes on x64; only the VT_LPWSTR case is read here.
type propVariant struct {
	Vt  uint16
	_   uint16
	_   uint16
	_   uint16
	Val uintptr
	_   uintptr
}

// call invokes vtable slot idx on a COM interface pointer.
//
// The uintptr-to-pointer conversions are flagged by go vet. They are correct
// here: COM interface pointers reference native memory that the Go garbage
// collector neither moves nor reclaims.
func call(this uintptr, idx int, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(unsafe.Pointer(this))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(idx)*unsafe.Sizeof(uintptr(0))))

	all := make([]uintptr, 0, len(args)+1)
	all = append(all, this)
	all = append(all, args...)

	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// InitCOM initialises COM on the calling thread and returns the teardown.
// The caller must already hold the thread via runtime.LockOSThread.
//
// CoUninitialize must balance a *successful* CoInitializeEx and nothing else.
// When the thread is already in a different apartment the call returns
// RPC_E_CHANGED_MODE, having taken no reference: uninitialising then would
// decrement a reference this package never took, tearing COM down under
// whoever did initialise the thread. The returned teardown is a no-op in that
// case.
func InitCOM() (func(), error) {
	hr, _, _ := _coInitializeEx.Call(0, _apartmentMode)

	if uint32(hr) == _rpcChangedMode {
		return func() {}, nil
	}
	if failed(hr) {
		return nil, fmt.Errorf("CoInitializeEx: 0x%08X", uint32(hr))
	}
	return func() { _coUninitialize.Call() }, nil
}

// Session holds a device enumerator for the lifetime of a polling loop.
//
// Recreating it per poll costs roughly 12ms, which showed up as a steady 1.2%
// of a CPU core in an otherwise idle background process.
type Session struct{ enum uintptr }

// NewSession creates the enumerator. Requires InitCOM on this thread.
func NewSession() (*Session, error) {
	var enum uintptr
	hr, _, _ := _coCreateInstance.Call(
		uintptr(unsafe.Pointer(&_clsidMMDeviceEnumerator)),
		0,
		_clsctxAll,
		uintptr(unsafe.Pointer(&_iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)),
	)
	if failed(hr) {
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): 0x%08X", uint32(hr))
	}
	return &Session{enum: enum}, nil
}

// Close releases the enumerator.
func (s *Session) Close() {
	if s == nil || s.enum == 0 {
		return
	}
	call(s.enum, _slotRelease)
	s.enum = 0
}

// ErrNoDefaultDevice reports that Windows has no default playback device,
// which happens transiently while devices are being switched.
var ErrNoDefaultDevice = errors.New("no default playback device")

// DefaultRender returns the current default playback device. The caller must
// Release it.
func (s *Session) DefaultRender() (*Device, error) {
	var dev uintptr
	if hr := call(s.enum, _slotGetDefaultAudioEndpoint, _eRender, _eMultimedia, uintptr(unsafe.Pointer(&dev))); failed(hr) {
		return nil, fmt.Errorf("%w: 0x%08X", ErrNoDefaultDevice, uint32(hr))
	}
	return &Device{ptr: dev}, nil
}

// Device is one audio endpoint.
type Device struct{ ptr uintptr }

// Release drops the reference.
func (d *Device) Release() {
	if d == nil || d.ptr == 0 {
		return
	}
	call(d.ptr, _slotRelease)
	d.ptr = 0
}

// ID returns the endpoint's stable identifier. Much cheaper than Name, so
// poll loops should compare IDs and only read the name when one changes.
func (d *Device) ID() (string, error) {
	var p uintptr
	if hr := call(d.ptr, _slotGetID, uintptr(unsafe.Pointer(&p))); failed(hr) {
		return "", fmt.Errorf("IMMDevice::GetId: 0x%08X", uint32(hr))
	}
	defer _coTaskMemFree.Call(p)
	return utf16PtrToString(p), nil
}

// Name returns the human-readable device name, such as
// "LG ULTRAGEAR+ (NVIDIA High Definition Audio)".
func (d *Device) Name() (string, error) {
	var store uintptr
	if hr := call(d.ptr, _slotOpenPropertyStore, 0, uintptr(unsafe.Pointer(&store))); failed(hr) {
		return "", fmt.Errorf("OpenPropertyStore: 0x%08X", uint32(hr))
	}
	defer call(store, _slotRelease)

	var pv propVariant
	if hr := call(store, _slotGetValue,
		uintptr(unsafe.Pointer(&_pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&pv))); failed(hr) {
		return "", fmt.Errorf("IPropertyStore::GetValue: 0x%08X", uint32(hr))
	}
	defer _propVariantClearFn.Call(uintptr(unsafe.Pointer(&pv)))

	if pv.Vt != _vtLPWSTR || pv.Val == 0 {
		return "", fmt.Errorf("friendly name is not a string (vt=%d)", pv.Vt)
	}
	return utf16PtrToString(pv.Val), nil
}

// Volume returns the endpoint's master level, 0.0 to 1.0.
func (d *Device) Volume() (float32, error) {
	var level float32
	err := d.withVolume(func(vol uintptr) error {
		if hr := call(vol, _slotGetMasterVolumeScalar, uintptr(unsafe.Pointer(&level))); failed(hr) {
			return fmt.Errorf("GetMasterVolumeLevelScalar: 0x%08X", uint32(hr))
		}
		return nil
	})
	return level, err
}

// PinToMax unmutes the endpoint and drives it to 100%, returning the level it
// started at.
//
// IAudioEndpointVolume::SetMasterVolumeLevelScalar takes a float, and the
// Win64 ABI passes floats in XMM registers, which Go's syscall package cannot
// populate. VolumeStepUp takes integer arguments only, so stepping to the top
// is the workaround. Reading the level back is fine: it is an out-parameter
// in memory rather than a register-passed float.
func (d *Device) PinToMax() (before float32, err error) {
	const maxSteps = 100

	err = d.withVolume(func(vol uintptr) error {
		read := func() (float32, error) {
			var v float32
			if hr := call(vol, _slotGetMasterVolumeScalar, uintptr(unsafe.Pointer(&v))); failed(hr) {
				return 0, fmt.Errorf("GetMasterVolumeLevelScalar: 0x%08X", uint32(hr))
			}
			return v, nil
		}

		var readErr error
		if before, readErr = read(); readErr != nil {
			return readErr
		}

		// A muted endpoint at 100% is still silent.
		var muted int32
		if hr := call(vol, _slotGetMute, uintptr(unsafe.Pointer(&muted))); !failed(hr) && muted != 0 {
			call(vol, _slotSetMute, 0, 0)
		}

		for range maxSteps {
			v, err := read()
			if err != nil {
				return err
			}
			if v >= 0.999 {
				return nil
			}
			if hr := call(vol, _slotVolumeStepUp, 0); failed(hr) {
				return fmt.Errorf("VolumeStepUp: 0x%08X", uint32(hr))
			}
		}
		return fmt.Errorf("could not reach 100%% in %d steps", maxSteps)
	})
	return before, err
}

func (d *Device) withVolume(fn func(vol uintptr) error) error {
	var vol uintptr
	if hr := call(d.ptr, _slotActivate,
		uintptr(unsafe.Pointer(&_iidIAudioEndpointVolume)), _clsctxAll, 0,
		uintptr(unsafe.Pointer(&vol))); failed(hr) {
		return fmt.Errorf("Activate(IAudioEndpointVolume): 0x%08X", uint32(hr))
	}
	defer call(vol, _slotRelease)
	return fn(vol)
}

func utf16PtrToString(p uintptr) string {
	if p == 0 {
		return ""
	}
	const maxLen = 4096

	var out []uint16
	for i := range maxLen {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i)*2))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return syscall.UTF16ToString(out)
}
