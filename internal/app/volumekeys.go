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

	"github.com/klaidliadon/deskmux/ddc"
	"github.com/klaidliadon/deskmux/vcp"
	"github.com/klaidliadon/deskmux/winaudio"
)

var (
	_user32         = syscall.NewLazyDLL("user32.dll")
	_setWindowsHook = _user32.NewProc("SetWindowsHookExW")
	_callNextHook   = _user32.NewProc("CallNextHookEx")
	_unhookWindows  = _user32.NewProc("UnhookWindowsHookEx")
	_getMessage     = _user32.NewProc("GetMessageW")
	_postThreadMsg  = _user32.NewProc("PostThreadMessageW")

	_kernel32      = syscall.NewLazyDLL("kernel32.dll")
	_getCurrentTID = _kernel32.NewProc("GetCurrentThreadId")
	_createMutex   = _kernel32.NewProc("CreateMutexW")
	_closeHandle   = _kernel32.NewProc("CloseHandle")
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

	// MCCS audio mute values.
	_muteOn  vcp.Level = 1
	_muteOff vcp.Level = 2
)

// ErrAlreadyRunning reports a second instance. Two keyboard hooks would both
// swallow every volume key and both write the monitor, which presents as
// erratic double-stepping rather than as an obvious failure.
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

// volumeState is the handoff between the hook thread, which must return
// promptly, and the writer, which performs slow DDC transactions.
type volumeState struct {
	mu       sync.Mutex
	target   int
	maxLevel int
	muted    bool
	dirty    bool
	muteReq  bool

	wake chan struct{}
}

func newVolumeState(level, maxLevel int) *volumeState {
	return &volumeState{
		target:   level,
		maxLevel: maxLevel,
		wake:     make(chan struct{}, 1),
	}
}

func (s *volumeState) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *volumeState) adjust(delta int) {
	s.mu.Lock()
	s.target = min(max(s.target+delta, 0), s.maxLevel)
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

// pending atomically takes the outstanding work and clears the flags.
func (s *volumeState) pending() (target int, wantLevel, wantMute, muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	target, wantLevel, wantMute, muted = s.target, s.dirty, s.muteReq, s.muted
	s.dirty, s.muteReq = false, false
	return target, wantLevel, wantMute, muted
}

// syncTo adopts a level observed on the monitor, unless a write is queued.
func (s *volumeState) syncTo(level int) (adopted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dirty || s.target == level {
		return false
	}
	s.target = level
	return true
}

// VolumeKeys makes the volume keys drive the monitor's own volume instead of
// the Windows endpoint, matching how macOS behaves on a display it cannot
// attenuate. Active only while the monitor is the default playback device.
func (a *App) VolumeKeys(ctx context.Context) error {
	release, err := singleInstance(`Local\deskmux-volumekeys`)
	if err != nil {
		return err
	}
	defer release()

	cfg := a.cfg.VolumeKeys
	if cfg.Step <= 0 {
		return fmt.Errorf("volume_keys.step must be positive, got %d", cfg.Step)
	}

	current, err := a.awaitVolumeReading(ctx)
	if err != nil {
		return err
	}

	maxLevel := int(current.Max)
	if maxLevel <= 0 {
		maxLevel = 100
	}

	state := newVolumeState(int(current.Current), maxLevel)
	a.log.Info("volume keys bound to monitor",
		"level", state.target, "max", maxLevel, "step", cfg.Step, "audio_match", cfg.AudioMatch)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Every goroutine is waited on before returning, so the hook is always
	// unhooked and the DDC handle always released.
	var (
		wg     sync.WaitGroup
		active atomic.Bool
	)

	wg.Go(func() { a.runVolumeWriter(ctx, state) })
	wg.Go(func() { a.newDeviceTracker(&active).run(ctx) })

	hook := newKeyHook(state, &active, cfg.Step)
	wg.Go(hook.run)

	// The hook thread parks in GetMessage, which returns only when a message
	// arrives, so shutdown must post WM_QUIT to it. Taking ctx.Done() without
	// waking the thread would leave it blocked and hang wg.Wait(), so every
	// exit path goes through hook.stop().
	var runErr error
	select {
	case <-hook.ready:
		a.log.Info("listening for volume keys")
		select {
		case <-ctx.Done():
		case err := <-hook.done:
			if err != nil {
				a.log.Error("keyboard hook ended", "err", err)
			}
		}
	case err := <-hook.done:
		runErr = err
	case <-ctx.Done():
	}

	hook.stop()
	cancel()
	wg.Wait()

	a.log.Info("volume keys released")
	return runErr
}

// awaitVolumeReading blocks until the monitor answers, or ctx is cancelled.
//
// A daemon started at logon routinely finds the monitor asleep, showing
// another input, or with its DDC engine wedged -- a state some panels enter
// after certain writes and leave only on a power cycle. Exiting then would
// mean the daemon is permanently gone by the time the display is usable, so
// it waits instead. Nothing is hooked until the monitor can actually be
// driven, which is the honest behaviour: without DDC there is nothing the
// volume keys could do.
func (a *App) awaitVolumeReading(ctx context.Context) (ddc.Reading, error) {
	const (
		firstDelay = 2 * time.Second
		maxDelay   = 30 * time.Second
	)

	delay := firstDelay
	for attempt := 1; ; attempt++ {
		set, monitor, err := a.openMonitor()
		if err == nil {
			reading, readErr := monitor.GetVCP(a.cfg.Registers.Volume)
			set.Close()
			if readErr == nil {
				if attempt > 1 {
					a.log.Info("monitor is answering again", "attempts", attempt)
				}
				return reading, nil
			}
			err = readErr
		}

		// Log the first failure plainly and the rest at debug, so a monitor
		// that is simply off overnight does not fill the log.
		if attempt == 1 {
			a.log.Warn("monitor not reachable, waiting", "err", err, "retry_in", delay)
		} else {
			a.log.Debug("monitor still not reachable", "attempt", attempt, "err", err)
		}

		select {
		case <-ctx.Done():
			return ddc.Reading{}, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

// keyHook owns the low-level keyboard hook and the message loop that keeps it
// alive. The hook must live on a single OS thread for its whole lifetime.
type keyHook struct {
	state  *volumeState
	active *atomic.Bool
	step   int

	ready chan uint32 // carries the thread id once the hook is installed
	done  chan error

	tid     uint32
	haveTID bool
}

func newKeyHook(state *volumeState, active *atomic.Bool, step int) *keyHook {
	return &keyHook{
		state:  state,
		active: active,
		step:   step,
		ready:  make(chan uint32, 1),
		done:   make(chan error, 1),
	}
}

func (h *keyHook) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	callback := syscall.NewCallback(h.onKey)

	handle, _, callErr := _setWindowsHook.Call(_whKeyboardLL, callback, 0, 0)
	if handle == 0 {
		h.done <- fmt.Errorf("SetWindowsHookEx: %w", callErr)
		return
	}
	defer _unhookWindows.Call(handle)

	tid, _, _ := _getCurrentTID.Call()
	h.ready <- uint32(tid)

	var msg winMsg
	for {
		r, _, _ := _getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			h.done <- nil
			return
		}
	}
}

// onKey must return promptly: a slow hook is silently unhooked by Windows. It
// only records the intent and wakes the writer, which does the DDC work.
func (h *keyHook) onKey(code int32, wParam, lParam uintptr) uintptr {
	// Only hijack the keys while the monitor is the playback device. On
	// headphones they must behave normally: Windows' own flyout and
	// per-endpoint volume are correct there, and the monitor's register would
	// be adjusting something nobody is listening to.
	if code == 0 && (wParam == _wmKeyDown || wParam == _wmSysKeyDown) && h.active.Load() {
		k := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
		switch k.VkCode {
		case _vkVolumeUp:
			h.state.adjust(h.step)
			return 1 // swallow
		case _vkVolumeDown:
			h.state.adjust(-h.step)
			return 1
		case _vkVolumeMute:
			h.state.toggleMute()
			return 1
		}
	}

	r, _, _ := _callNextHook.Call(0, uintptr(code), wParam, lParam)
	return r
}

// stop wakes the message loop so run can unhook and return.
func (h *keyHook) stop() {
	if !h.haveTID {
		select {
		case h.tid = <-h.ready:
			h.haveTID = true
		default:
		}
	}
	if h.haveTID {
		_postThreadMsg.Call(uintptr(h.tid), _wmQuit, 0, 0)
	}
}

// volumeSession keeps one DDC handle open for the writer's lifetime.
//
// Re-enumerating per write (EnumDisplayMonitors, GetPhysicalMonitors,
// Destroy) costs tens of milliseconds and was the dominant source of input
// lag when the keys felt sluggish.
type volumeSession struct {
	app *App
	set *ddc.Set
	mon ddc.Monitor
}

func (s *volumeSession) open() error {
	set, mon, err := s.app.openMonitor()
	if err != nil {
		return err
	}
	s.set, s.mon = set, mon
	return nil
}

func (s *volumeSession) close() {
	if s.set != nil {
		s.set.Close()
		s.set = nil
	}
}

func (s *volumeSession) ready() bool { return s.set != nil }

// write reopens the handle once if it has gone stale, which happens whenever
// the panel's DDC engine drops out and comes back.
func (s *volumeSession) write(code vcp.Code, value vcp.Level) error {
	if s.set == nil {
		if err := s.open(); err != nil {
			return err
		}
	}
	if err := s.mon.SetVCPOnce(code, value); err != nil {
		s.close()
		if err := s.open(); err != nil {
			return err
		}
		return s.mon.SetVCPOnce(code, value)
	}
	return nil
}

func (s *volumeSession) read(code vcp.Code) (ddc.Reading, error) {
	if s.set == nil {
		return ddc.Reading{}, ddc.ErrNoMonitors
	}
	return s.mon.GetVCP(code)
}

// runVolumeWriter writes on the leading edge so a single key press is never
// delayed, then rate-limits while a key is held.
func (a *App) runVolumeWriter(ctx context.Context, state *volumeState) {
	session := &volumeSession{app: a}
	defer session.close()

	if err := session.open(); err != nil {
		a.log.Error("cannot open DDC session", "err", err)
	}

	resync := time.NewTicker(a.cfg.VolumeKeys.Resync.D())
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-resync.C:
			a.resyncVolume(session, state)
			continue

		case <-state.wake:
		}

		target, wantLevel, wantMute, muted := state.pending()

		if wantMute {
			value := _muteOff
			if muted {
				value = _muteOn
			}
			if err := session.write(a.cfg.Registers.Mute, value); err != nil {
				a.log.Warn("mute write failed", "err", err)
			} else {
				a.log.Info("mute", "muted", muted)
			}
		}

		if wantLevel {
			if err := session.write(a.cfg.Registers.Volume, vcp.Level(target)); err != nil {
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

// resyncVolume adopts a level set by something else: another deskmux call, a
// dock profile, or the other machine while it had the panel.
func (a *App) resyncVolume(session *volumeSession, state *volumeState) {
	if !session.ready() {
		return
	}

	reading, err := session.read(a.cfg.Registers.Volume)
	if err != nil {
		return
	}
	if state.syncTo(int(reading.Current)) {
		a.log.Debug("volume changed externally, resynced", "level", reading.Current)
	}
}

// deviceTracker follows the default playback device and owns endpoint
// pinning, which applies only while the monitor is that device.
type deviceTracker struct {
	app    *App
	active *atomic.Bool
	match  string

	lastID  string
	first   bool
	matched bool
	since   int
}

func (a *App) newDeviceTracker(active *atomic.Bool) *deviceTracker {
	return &deviceTracker{
		app:    a,
		active: active,
		match:  strings.ToUpper(a.cfg.VolumeKeys.AudioMatch),
		first:  true,
	}
}

func (t *deviceTracker) run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	uninit, err := winaudio.InitCOM()
	if err != nil {
		t.app.log.Error("audio watch disabled", "err", err)
		return
	}
	defer uninit()

	session, err := winaudio.NewSession()
	if err != nil {
		t.app.log.Error("audio watch disabled", "err", err)
		return
	}
	defer session.Close()

	ticker := time.NewTicker(t.app.cfg.VolumeKeys.AudioPoll.D())
	defer ticker.Stop()

	for {
		t.poll(session)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// driftChecks is how many polls pass between endpoint-level checks. Reading
// the level is cheap but not free, and drift is rare.
const driftChecks = 10

// poll compares device IDs, which is far cheaper than reading friendly names,
// and pays for the name only when the device actually changed.
func (t *deviceTracker) poll(session *winaudio.Session) {
	device, err := session.DefaultRender()
	if err != nil {
		return
	}
	defer device.Release()

	id, err := device.ID()
	if err != nil {
		return
	}

	if id != t.lastID || t.first {
		t.onDeviceChanged(device, id)
		return
	}

	if !t.matched || !t.app.cfg.VolumeKeys.PinWindows {
		return
	}
	if t.since++; t.since < driftChecks {
		return
	}
	t.since = 0

	if level, err := device.Volume(); err == nil && level < 0.999 {
		if _, err := device.PinToMax(); err == nil {
			t.app.log.Info("windows endpoint drifted, re-pinned", "was", percent(level))
		}
	}
}

func (t *deviceTracker) onDeviceChanged(device *winaudio.Device, id string) {
	name, err := device.Name()
	matched := err == nil && strings.Contains(strings.ToUpper(name), t.match)
	t.active.Store(matched)

	if !t.first {
		if matched {
			t.app.log.Info("playback device changed: volume keys drive the monitor", "device", name)
		} else {
			t.app.log.Info("playback device changed: volume keys handed back to Windows", "device", name)
		}
	}

	if matched && t.app.cfg.VolumeKeys.PinWindows {
		if before, err := device.PinToMax(); err == nil && before < 0.999 {
			t.app.log.Info("windows endpoint pinned to 100%", "was", percent(before))
		}
	}

	t.lastID, t.matched, t.first, t.since = id, matched, false, 0
}

func percent(v float32) string { return fmt.Sprintf("%.0f%%", v*100) }

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
	if errno, ok := errors.AsType[syscall.Errno](lastErr); ok && errno == _errorAlreadyExists {
		_closeHandle.Call(handle)
		return nil, ErrAlreadyRunning
	}
	return func() { _closeHandle.Call(handle) }, nil
}
