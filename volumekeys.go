package main

// Volume-key interception.
//
// Makes Windows behave like the Mac: the endpoint sits pinned at 100% and the
// volume keys drive the monitor's own hardware volume (VCP 0x62) instead.
// With both machines then using 0x62 as the single volume control, the level
// is machine-independent and no handoff is needed when the dock moves.
//
// The keys are swallowed, so Windows' own volume flyout never appears. The
// monitor draws its own OSD when 0x62 changes, which is the intended feedback.
//
// A low-level keyboard hook must return promptly or Windows silently unhooks
// it, and a DDC write takes 50-250ms. So the hook only records the new target
// and wakes a worker; the worker coalesces bursts (holding a volume key
// repeats fast) into a single write. That also protects the panel's DDC
// engine, which wedges if hammered.

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	procSetWindowsHookEx   = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx     = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHook  = user32.NewProc("UnhookWindowsHookEx")
	procGetMessage         = user32.NewProc("GetMessageW")
	procPostThreadMessage  = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
	procCloseHandle = kernel32.NewProc("CloseHandle")
)

const errorAlreadyExists = 183

// singleInstance stops a second copy from running. Two keyboard hooks would
// both swallow every volume key and both write the monitor, which reads as
// erratic double-stepping rather than an obvious failure -- worth preventing
// outright rather than debugging later.
func singleInstance(name string) (release func(), err error) {
	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, lastErr := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return nil, fmt.Errorf("CreateMutex failed: %v", lastErr)
	}
	if errno, ok := lastErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		procCloseHandle.Call(h)
		return nil, fmt.Errorf("another instance is already running")
	}
	return func() { procCloseHandle.Call(h) }, nil
}

const (
	whKeyboardLL = 13

	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104
	wmQuit       = 0x0012

	vkVolumeMute = 0xAD
	vkVolumeDown = 0xAE
	vkVolumeUp   = 0xAF
)

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// volumeState is shared between the hook thread and the writer goroutine.
type volumeState struct {
	mu      sync.Mutex
	target  int  // 0..100, the level we want on the monitor
	muted   bool // our tracked mute state (VCP 0x8D)
	dirty   bool
	muteReq bool
	wake    chan struct{}
}

func (s *volumeState) adjust(delta int, max int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target += delta
	if s.target < 0 {
		s.target = 0
	}
	if s.target > max {
		s.target = max
	}
	s.dirty = true
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.target
}

func (s *volumeState) toggleMute() {
	s.mu.Lock()
	s.muted = !s.muted
	s.muteReq = true
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func cmdVolumeKeys(args []string) error {
	step := *flagStep
	if step <= 0 {
		return fmt.Errorf("-step must be positive")
	}

	release, err := singleInstance("Local\\lginput-volumekeys")
	if err != nil {
		return err
	}
	defer release()

	// Establish the starting level from the monitor itself.
	ms, m, err := pickMonitor()
	if err != nil {
		return fmt.Errorf("cannot reach the monitor over DDC: %w", err)
	}
	cur, err := m.GetVCP(vcpVolume)
	ms.Close()
	if err != nil {
		return fmt.Errorf("cannot read volume (0x62): %w", err)
	}
	maxVol := int(cur.Max)
	if maxVol <= 0 {
		maxVol = 100
	}

	st := &volumeState{target: int(cur.Current), wake: make(chan struct{}, 1)}

	logf("volume keys -> monitor VCP 0x62\n")
	logf("  monitor volume : %d (max %d)\n", st.target, maxVol)
	logf("  step           : %d\n", step)

	if name, err := DefaultRenderName(); err == nil {
		logf("  playback device: %s\n", name)
	}
	logf("  active only when the playback device matches %q\n", *flagAudioMatch)
	logln()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go volumeWriter(st, maxVol, done)
	go trackDefaultDevice(done)

	hookErr := make(chan error, 1)
	threadID := make(chan uint32, 1)
	go runKeyboardHook(st, step, maxVol, hookErr, threadID)

	var tid uint32
	select {
	case tid = <-threadID:
	case err := <-hookErr:
		close(done)
		return err
	case <-time.After(5 * time.Second):
		close(done)
		return fmt.Errorf("keyboard hook did not start")
	}

	logf("[%s] listening. Press Ctrl+C to stop.\n", ts())

	select {
	case <-stop:
		logf("\n[%s] stopping\n", ts())
	case err := <-hookErr:
		logf("\n[%s] hook ended: %v\n", ts(), err)
	}
	procPostThreadMessage.Call(uintptr(tid), wmQuit, 0, 0)
	close(done)
	time.Sleep(200 * time.Millisecond)
	return nil
}

// monitorIsDefault is read by the hook callback, which must not block, so the
// default-device check is done out of band and cached here.
var monitorIsDefault atomic.Bool

// trackDefaultDevice follows the default playback device and owns the
// endpoint pinning, which only applies while the monitor is that device.
func trackDefaultDevice(done <-chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, 2)
	if int32(hr) < 0 && uint32(hr) != 0x80010106 {
		logf("[%s] audio watch disabled: CoInitializeEx 0x%08X\n", ts(), uint32(hr))
		return
	}
	defer procCoUninitialize.Call()

	sess, err := newAudioSession()
	if err != nil {
		logf("[%s] audio watch disabled: %v\n", ts(), err)
		return
	}
	defer sess.close()

	match := strings.ToUpper(*flagAudioMatch)
	tick := time.NewTicker(*flagAudioPoll)
	defer tick.Stop()

	var lastID string
	var last bool
	first := true
	var since int

	for {
		dev, err := sess.defaultDevice()
		if err == nil {
			id, idErr := deviceID(dev)
			// Only pay for the friendly-name lookup when the device changed.
			if idErr == nil && (id != lastID || first) {
				name, nameErr := deviceName(dev)
				now := nameErr == nil && strings.Contains(strings.ToUpper(name), match)
				monitorIsDefault.Store(now)
				if !first {
					if now {
						logf("[%s] playback -> %q: volume keys now drive the monitor\n", ts(), name)
					} else {
						logf("[%s] playback -> %q: volume keys handed back to Windows\n", ts(), name)
					}
				}
				if now && *flagPinWindows {
					if before, err := pinDevice(dev); err == nil && before < 0.999 {
						logf("[%s] windows endpoint pinned to 100%% (was %.0f%%)\n", ts(), before*100)
					}
				}
				lastID, last, first, since = id, now, false, 0
			} else if last && *flagPinWindows {
				// Occasional drift check; other apps can move the endpoint.
				if since++; since >= 10 {
					since = 0
					if v, err := deviceVolume(dev); err == nil && v < 0.999 {
						if _, err := pinDevice(dev); err == nil {
							logf("[%s] windows endpoint drifted to %.0f%%, re-pinned\n", ts(), v*100)
						}
					}
				}
			}
			comCall(dev, slotRelease)
		}

		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

// runKeyboardHook owns the hook and its message loop. It must stay on one OS
// thread for the lifetime of the hook.
func runKeyboardHook(st *volumeState, step, maxVol int, errc chan<- error, tidc chan<- uint32) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cb := syscall.NewCallback(func(nCode int32, wParam uintptr, lParam uintptr) uintptr {
		// Only hijack the keys while the monitor is the playback device.
		// On headphones or laptop speakers the keys must behave normally --
		// Windows' own flyout and per-endpoint volume are correct there, and
		// 0x62 would be adjusting something nobody is listening to.
		if nCode == 0 && (wParam == wmKeyDown || wParam == wmSysKeyDown) && monitorIsDefault.Load() {
			k := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
			switch k.VkCode {
			case vkVolumeUp:
				st.adjust(step, maxVol)
				return 1 // swallow
			case vkVolumeDown:
				st.adjust(-step, maxVol)
				return 1
			case vkVolumeMute:
				st.toggleMute()
				return 1
			}
		}
		r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return r
	})

	hook, _, err := procSetWindowsHookEx.Call(whKeyboardLL, cb, 0, 0)
	if hook == 0 {
		errc <- fmt.Errorf("SetWindowsHookEx failed: %v", err)
		return
	}
	defer procUnhookWindowsHook.Call(hook)

	tid, _, _ := procGetCurrentThreadId.Call()
	tidc <- uint32(tid)

	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			errc <- nil
			return
		}
	}
}

// ddcSession keeps one physical-monitor handle open. Re-enumerating per write
// (EnumDisplayMonitors + GetPhysicalMonitorsFromHMONITOR + Destroy) costs tens
// of milliseconds and is the single biggest source of input lag here.
type ddcSession struct {
	ms *Monitors
	m  Monitor
}

func openSession() (*ddcSession, error) {
	ms, m, err := pickMonitor()
	if err != nil {
		return nil, err
	}
	return &ddcSession{ms: ms, m: m}, nil
}

func (s *ddcSession) close() {
	if s != nil && s.ms != nil {
		s.ms.Close()
	}
}

// volumeWriter writes on the leading edge -- the first keystroke goes out
// immediately -- then rate-limits while the key is held, applying whatever
// total the repeats have accumulated.
func volumeWriter(st *volumeState, maxVol int, done <-chan struct{}) {
	sess, err := openSession()
	if err != nil {
		logf("[%s] cannot open DDC session: %v\n", ts(), err)
	}
	defer sess.close()

	// write applies one value, reopening the handle once if it has gone stale.
	write := func(code byte, value uint32) error {
		if sess == nil {
			if sess, err = openSession(); err != nil {
				return err
			}
		}
		if err := sess.m.SetVCPOnce(code, value); err != nil {
			sess.close()
			sess = nil
			if sess, err = openSession(); err != nil {
				return err
			}
			return sess.m.SetVCPOnce(code, value)
		}
		return nil
	}

	// Anything else can move 0x62 behind our back -- `lginput volume`, the
	// dock handoff, or the Mac while it had the panel. Re-read periodically
	// so the next key press steps from the real level, not a stale one.
	resync := time.NewTicker(10 * time.Second)
	defer resync.Stop()

	for {
		select {
		case <-done:
			return
		case <-resync.C:
			if sess == nil {
				continue
			}
			v, err := sess.m.GetVCP(vcpVolume)
			if err != nil {
				continue
			}
			st.mu.Lock()
			if !st.dirty && int(v.Current) != st.target {
				st.target = int(v.Current)
			}
			st.mu.Unlock()
			continue
		case <-st.wake:
		}

		st.mu.Lock()
		target, dirty, muteReq, muted := st.target, st.dirty, st.muteReq, st.muted
		st.dirty, st.muteReq = false, false
		st.mu.Unlock()

		if muteReq {
			v := uint32(2) // 1 = mute, 2 = unmute
			if muted {
				v = 1
			}
			if err := write(vcpMute, v); err != nil {
				logf("[%s] mute: %v\n", ts(), err)
			} else {
				logf("[%s] mute -> %v\n", ts(), muted)
			}
		}

		if dirty {
			if err := write(vcpVolume, uint32(target)); err != nil {
				logf("[%s] volume %d: %v\n", ts(), target, err)
			} else if *flagVerbose {
				logf("[%s] volume -> %d\n", ts(), target)
			}
		}

		// Rate-limit only *after* writing, so a single press is not delayed.
		time.Sleep(*flagCoalesce)
	}
}
