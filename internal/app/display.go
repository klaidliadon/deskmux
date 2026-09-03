package app

import (
	"fmt"
	"time"
	"unsafe"
)

var (
	_mouseEvent       = _user32.NewProc("mouse_event")
	_getLastInputInfo = _user32.NewProc("GetLastInputInfo")

	_getTickCount64          = _kernel32.NewProc("GetTickCount64")
	_setThreadExecutionState = _kernel32.NewProc("SetThreadExecutionState")
)

const (
	_mouseEventMove = 0x0001

	// ES_DISPLAY_REQUIRED without ES_CONTINUOUS resets the display idle timer
	// once and returns. The continuous form would keep the display awake until
	// explicitly cleared, which is not what a one-shot wake wants.
	_esDisplayRequired = 0x00000002
)

type lastInputInfo struct {
	CbSize uint32
	DwTime uint32
}

// idleTime is how long since the last real or synthetic user input.
//
// Windows measures this against GetTickCount, so both values are 32-bit
// milliseconds and the subtraction is done in uint32 to wrap correctly at the
// 49-day rollover rather than producing a nonsense duration.
func idleTime() (time.Duration, error) {
	info := lastInputInfo{CbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}

	r, _, err := _getLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}

	ticks, _, _ := _getTickCount64.Call()
	return time.Duration(uint32(ticks)-info.DwTime) * time.Millisecond, nil
}

// wakeDisplay turns this machine's own video output back on, and reports how
// long the machine had been idle first.
//
// This is the other half of switching the panel to this machine. Windows
// blanks its output after the display timeout -- five minutes by default --
// while the panel itself stays lit showing whatever else is plugged into it.
// Grabbing the panel in that state selects an input carrying no signal, so
// the handover looks like it silently failed.
//
// The wake itself is a zero-delta mouse move. Synthetic input is the
// dependable way to bring a display back on Windows 10 and later, and
// zero-delta means it wakes the display without moving the pointer.
// SetThreadExecutionState then resets the display idle timer, so the wake is
// not immediately undone by a timeout that was already about to expire.
//
// Notably absent: broadcasting WM_SYSCOMMAND/SC_MONITORPOWER, which is the
// usual advice. SendMessage to HWND_BROADCAST blocks until every top-level
// window has processed the message, and SendMessageTimeout does not bound
// that -- its timeout applies per window, not to the call. Measured here, one
// such broadcast took 131 seconds against a 1000ms timeout. Neither belongs
// anywhere near a daemon, and neither is needed.
func wakeDisplay() (time.Duration, error) {
	idle, err := idleTime()
	if err != nil {
		return 0, err
	}

	// Neither call fails in a way worth reporting: mouse_event returns void,
	// and SetThreadExecutionState returns the previous state.
	_mouseEvent.Call(_mouseEventMove, 0, 0, 0, 0)
	_setThreadExecutionState.Call(_esDisplayRequired)

	return idle, nil
}

// Wake turns this machine's display output back on.
func (a *App) Wake() error {
	if a.opts.DryRun {
		a.println("dry-run: would wake this machine's display")
		return nil
	}

	idle, err := a.wake()
	if err != nil {
		return err
	}

	a.printf("display woken (idle for %s)\n", idle.Round(time.Second))
	return nil
}

// applyWake is the profile step. Failing to wake must not stop the handover:
// the input switch is still worth making, and the user can nudge the mouse.
func (a *App) applyWake(event string) {
	if a.opts.DryRun {
		a.log.Info("profile wake", "event", event, "result", "dry-run")
		return
	}

	idle, err := a.wake()
	if err != nil {
		a.log.Warn("display wake failed", "event", event, "err", err)
		return
	}
	a.log.Info("display woken", "event", event, "idle", idle.Round(time.Second))
}
