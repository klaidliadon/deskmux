package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/klaidliadon/lginput/ddc"
	"github.com/klaidliadon/lginput/vcp"
	"github.com/klaidliadon/lginput/winaudio"
)

var (
	_user32         = syscall.NewLazyDLL("user32.dll")
	_setWindowsHook = _user32.NewProc("SetWindowsHookExW")
	_callNextHook   = _user32.NewProc("CallNextHookEx")
	_unhookWindows  = _user32.NewProc("UnhookWindowsHookEx")
	_getMessage     = _user32.NewProc("GetMessageW")
	_postThreadMsg  = _user32.NewProc("PostThreadMessageW")
	_kernel32       = syscall.NewLazyDLL("kernel32.dll")
	_getCurrentTID  = _kernel32.NewProc("GetCurrentThreadId")
	_createMutex    = _kernel32.NewProc("CreateMutexW")
	_closeHandleFn  = _kernel32.NewProc("CloseHandle")
)

const (
	_whKeyboardLL = 13

	_wmKeyDown    = 0x0100
	_wmSysKeyDown = 0x0104
	_wmQuit       = 0x0012

	_vkVolumeMute = 0xAD
	_vkVolumeDown = 0xAE
	_vkVolumeUp   = 0xAF

	_errorAlreadyExists = 183
)

// ErrAlreadyRunning reports a second instance. Two keyboard hooks would both
// swallow every volume key and both write the monitor, which presents as
// erratic double-stepping rather than an obvious failure.
var ErrAlreadyRunning = errors.New("another instance is already running")

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type winMsg struct {
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
	target  int
	muted   bool
	dirty   bool
	muteReq bool

	wake chan struct{}
}

func (s *volumeState) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *volumeState) adjust(delta, maxLevel int) {
	s.mu.Lock()
	s.target = min(max(s.target+delta, 0), maxLevel)
	s.dirty = true
	s.mu.Unlock()
	s.nudge()
}

func (s *volumeState) toggleMute() {
	s.mu.Lock()
	s.muted = !s.muted
	s.muteReq = true
	s.mu.Unlock()
	s.nudge()
}

// VolumeKeys makes the volume keys drive the monitor's own volume instead of
// the Windows endpoint, matching how macOS behaves on a display it cannot
// attenuate. Active only while the monitor is the default playback device.
func (a *App) VolumeKeys(ctx context.Context) error {
	release, err := singleInstance(`Local\lginput-volumekeys`)
	if err != nil {
		return err
	}
	defer release()

	cfg := a.cfg.VolumeKeys
	if cfg.Step <= 0 {
		return fmt.Errorf("volume_keys.step must be positive, got %d", cfg.Step)
	}

	set, m, err := a.openMonitor()
	if err != nil {
		return fmt.Errorf("reach the monitor over DDC: %w", err)
	}
	cur, err := m.GetVCP(a.cfg.Registers.Volume)
	set.Close()
	if err != nil {
		return fmt.Errorf("read volume register: %w", err)
	}

	maxLevel := int(cur.Max)
	if maxLevel <= 0 {
		maxLevel = 100
	}

	state := &volumeState{target: int(cur.Current), wake: make(chan struct{}, 1)}
	a.log.Info("volume keys bound to monitor",
		"level", state.target, "max", maxLevel, "step", cfg.Step, "audio_match", cfg.AudioMatch)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Goroutines are supervised rather than fired and forgotten: every one is
	// waited on before returning, so the hook is always unhooked cleanly.
	var wg sync.WaitGroup
	var active atomic.Bool

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.runVolumeWriter(ctx, state, maxLevel)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		a.trackPlaybackDevice(ctx, &active)
	}()

	hookErr := make(chan error, 1)
	threadID := make(chan uint32, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runKeyboardHook(ctx, state, &active, cfg.Step, maxLevel, hookErr, threadID)
	}()

	// The hook thread parks in GetMessage, which only returns when a message
	// arrives, so shutdown must post WM_QUIT to it. Every exit path below
	// therefore runs through stopHook: taking ctx.Done() here without waking
	// the thread would leave it blocked forever and hang wg.Wait().
	var (
		tid     uint32
		haveTID bool
		runErr  error
	)

	stopHook := func() {
		if !haveTID {
			// The hook may have started between the select and here.
			select {
			case tid = <-threadID:
				haveTID = true
			default:
			}
		}
		if haveTID {
			_postThreadMsg.Call(uintptr(tid), _wmQuit, 0, 0)
		}
		cancel()
		wg.Wait()
	}

	select {
	case tid = <-threadID:
		haveTID = true
	case err := <-hookErr:
		runErr = err
	case <-ctx.Done():
	}

	if runErr == nil && haveTID {
		a.log.Info("listening for volume keys")
		select {
		case <-ctx.Done():
		case err := <-hookErr:
			if err != nil {
				a.log.Error("keyboard hook ended", "err", err)
			}
		}
	}

	stopHook()
	a.log.Info("volume keys released")
	return runErr
}

// runKeyboardHook owns the hook and its message loop, and must stay on one OS
// thread for the hook's lifetime.
func runKeyboardHook(
	ctx context.Context,
	state *volumeState,
	active *atomic.Bool,
	step, maxLevel int,
	errc chan<- error,
	tidc chan<- uint32,
) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	callback := syscall.NewCallback(func(code int32, wParam, lParam uintptr) uintptr {
		// Only hijack the keys while the monitor is the playback device. On
		// headphones the keys must behave normally: Windows' own flyout and
		// per-endpoint volume are correct there, and the monitor's register
		// would be adjusting something nobody is listening to.
		if code == 0 && (wParam == _wmKeyDown || wParam == _wmSysKeyDown) && active.Load() {
			k := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
			switch k.VkCode {
			case _vkVolumeUp:
				state.adjust(step, maxLevel)
				return 1 // swallow
			case _vkVolumeDown:
				state.adjust(-step, maxLevel)
				return 1
			case _vkVolumeMute:
				state.toggleMute()
				return 1
			}
		}
		r, _, _ := _callNextHook.Call(0, uintptr(code), wParam, lParam)
		return r
	})

	hook, _, callErr := _setWindowsHook.Call(_whKeyboardLL, callback, 0, 0)
	if hook == 0 {
		errc <- fmt.Errorf("SetWindowsHookEx: %w", callErr)
		return
	}
	defer _unhookWindows.Call(hook)

	tid, _, _ := _getCurrentTID.Call()
	tidc <- uint32(tid)

	var msg winMsg
	for {
		r, _, _ := _getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			errc <- nil
			return
		}
		if ctx.Err() != nil {
			errc <- nil
			return
		}
	}
}

// runVolumeWriter writes on the leading edge so a single key press is not
// delayed, then rate-limits while a key is held.
//
// The DDC handle is held open for the whole loop: re-enumerating per write
// costs tens of milliseconds and was the dominant source of input lag.
func (a *App) runVolumeWriter(ctx context.Context, state *volumeState, maxLevel int) {
	var (
		set *ddc.Set
		mon ddc.Monitor
	)

	open := func() error {
		s, m, err := a.openMonitor()
		if err != nil {
			return err
		}
		set, mon = s, m
		return nil
	}
	closeSession := func() {
		if set != nil {
			set.Close()
			set = nil
		}
	}
	defer closeSession()

	if err := open(); err != nil {
		a.log.Error("cannot open DDC session", "err", err)
	}

	// write reopens the handle once if it has gone stale, which happens when
	// the panel's DDC engine drops out and comes back.
	write := func(code vcp.Code, value vcp.Level) error {
		if set == nil {
			if err := open(); err != nil {
				return err
			}
		}
		if err := mon.SetVCPOnce(code, value); err != nil {
			closeSession()
			if err := open(); err != nil {
				return err
			}
			return mon.SetVCPOnce(code, value)
		}
		return nil
	}

	resync := time.NewTicker(a.cfg.VolumeKeys.Resync.D())
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-resync.C:
			// Anything else can move the register behind our back: another
			// lginput call, a profile handoff, or the other machine while it
			// had the panel. Re-read so the next key press steps from the
			// real level rather than a stale one.
			if set == nil {
				continue
			}
			v, err := mon.GetVCP(a.cfg.Registers.Volume)
			if err != nil {
				continue
			}
			state.mu.Lock()
			if !state.dirty && int(v.Current) != state.target {
				a.log.Debug("volume changed externally", "was", state.target, "now", v.Current)
				state.target = int(v.Current)
			}
			state.mu.Unlock()
			continue

		case <-state.wake:
		}

		state.mu.Lock()
		target, dirty, muteReq, muted := state.target, state.dirty, state.muteReq, state.muted
		state.dirty, state.muteReq = false, false
		state.mu.Unlock()

		if muteReq {
			value := vcp.Level(2) // 1 mutes, 2 unmutes
			if muted {
				value = 1
			}
			if err := write(a.cfg.Registers.Mute, value); err != nil {
				a.log.Warn("mute write failed", "err", err)
			} else {
				a.log.Info("mute", "muted", muted)
			}
		}

		if dirty {
			if err := write(a.cfg.Registers.Volume, vcp.Level(target)); err != nil {
				a.log.Warn("volume write failed", "level", target, "err", err)
			} else {
				a.log.Debug("volume", "level", target)
			}
		}

		// Rate-limit after writing, never before, so one press is immediate.
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.cfg.VolumeKeys.Coalesce.D()):
		}
	}
}

// trackPlaybackDevice follows the default playback device and owns endpoint
// pinning, which applies only while the monitor is that device.
func (a *App) trackPlaybackDevice(ctx context.Context, active *atomic.Bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	uninit, err := winaudio.InitCOM()
	if err != nil {
		a.log.Error("audio watch disabled", "err", err)
		return
	}
	defer uninit()

	session, err := winaudio.NewSession()
	if err != nil {
		a.log.Error("audio watch disabled", "err", err)
		return
	}
	defer session.Close()

	cfg := a.cfg.VolumeKeys
	match := strings.ToUpper(cfg.AudioMatch)

	ticker := time.NewTicker(cfg.AudioPoll.D())
	defer ticker.Stop()

	const driftEvery = 10

	var lastID string
	first := true
	var matched bool
	var since int

	for {
		a.pollPlaybackDevice(session, match, &lastID, &first, &matched, &since, driftEvery, active)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollPlaybackDevice compares device IDs, which is far cheaper than reading
// friendly names, and only pays for the name when the device actually changed.
func (a *App) pollPlaybackDevice(
	session *winaudio.Session,
	match string,
	lastID *string,
	first *bool,
	matched *bool,
	since *int,
	driftEvery int,
	active *atomic.Bool,
) {
	dev, err := session.DefaultRender()
	if err != nil {
		return
	}
	defer dev.Release()

	id, err := dev.ID()
	if err != nil {
		return
	}

	if id != *lastID || *first {
		name, err := dev.Name()
		nowMatched := err == nil && strings.Contains(strings.ToUpper(name), match)
		active.Store(nowMatched)

		if !*first {
			if nowMatched {
				a.log.Info("playback device changed: volume keys drive the monitor", "device", name)
			} else {
				a.log.Info("playback device changed: volume keys handed back to Windows", "device", name)
			}
		}

		if nowMatched && a.cfg.VolumeKeys.PinWindows {
			if before, err := dev.PinToMax(); err == nil && before < 0.999 {
				a.log.Info("windows endpoint pinned to 100%", "was", fmt.Sprintf("%.0f%%", before*100))
			}
		}

		*lastID, *matched, *first, *since = id, nowMatched, false, 0
		return
	}

	if !*matched || !a.cfg.VolumeKeys.PinWindows {
		return
	}
	if *since++; *since < driftEvery {
		return
	}
	*since = 0

	if v, err := dev.Volume(); err == nil && v < 0.999 {
		if _, err := dev.PinToMax(); err == nil {
			a.log.Info("windows endpoint drifted, re-pinned", "was", fmt.Sprintf("%.0f%%", v*100))
		}
	}
}

// singleInstance takes a named mutex for the lifetime of the process.
func singleInstance(name string) (release func(), err error) {
	ptr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}

	handle, _, lastErr := _createMutex.Call(0, 0, uintptr(unsafe.Pointer(ptr)))
	if handle == 0 {
		return nil, fmt.Errorf("CreateMutex: %w", lastErr)
	}
	if errno, ok := lastErr.(syscall.Errno); ok && errno == _errorAlreadyExists {
		_closeHandleFn.Call(handle)
		return nil, ErrAlreadyRunning
	}
	return func() { _closeHandleFn.Call(handle) }, nil
}
