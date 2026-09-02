package main

import (
	"flag"
	"fmt"
	"io"
	"time"
)

// app holds every tunable and the output sink.
//
// Uber style: "Avoid Mutable Globals". The flag values were previously ~25
// package-level pointers that any function could read, which made the data
// flow invisible and the commands untestable. They are now injected.
type app struct {
	out io.Writer

	// global
	monitor int
	dryRun  bool
	verbose bool
	fast    bool
	useStd  bool
	src     string
	settle  time.Duration
	bus     time.Duration
	vol     int
	logPath string

	// watch
	match       string
	poll        time.Duration
	dockInput   string
	dockVol     int
	undockInput string
	undockVol   int
	undockPower bool

	// volumekeys
	step       int
	pinWindows bool
	coalesce   time.Duration
	audioMatch string
	audioPoll  time.Duration
}

func (a *app) logf(format string, args ...any) { fmt.Fprintf(a.out, format, args...) }
func (a *app) logln(args ...any)               { fmt.Fprintln(a.out, args...) }

// registerFlags binds a to fs. Kept separate from parsing so tests can build
// an app directly without going through the command line.
func (a *app) registerFlags(fs *flag.FlagSet) {
	fs.IntVar(&a.monitor, "m", 0, "monitor index (see `list`)")
	fs.BoolVar(&a.dryRun, "n", false, "dry run: show what would be sent, send nothing")
	fs.BoolVar(&a.verbose, "v", false, "verbose output")
	fs.BoolVar(&a.fast, "fast", false, "`input`/`raw`: skip the port sweep, send only per-output-bit writes")
	fs.BoolVar(&a.useStd, "std", false, "`input`: use the standard DDC path (source 0x51) instead of NVAPI 0x50")
	fs.StringVar(&a.src, "src", "", "override the DDC source address for `input`/`raw` (e.g. 0x50)")
	fs.DurationVar(&a.settle, "settle", 250*time.Millisecond, "delay before a verification read-back")
	fs.DurationVar(&a.bus, "bus", 40*time.Millisecond, "delay between raw I2C writes")
	fs.IntVar(&a.vol, "vol", -1, "`input`: also set monitor volume (0x62) as part of the switch")
	fs.StringVar(&a.logPath, "log", "", "also append all output to this `file`")

	fs.StringVar(&a.match, "match", _defaultDockMatch,
		"`watch`: comma-separated device-ID substrings identifying the dock")
	fs.DurationVar(&a.poll, "poll", 2*time.Second, "`watch`: how often to scan for device changes")
	fs.StringVar(&a.dockInput, "dock-input", "usb-c", "`watch`: input to select when the dock connects")
	fs.IntVar(&a.dockVol, "dock-vol", -1, "`watch`: monitor volume on connect (-1 = leave alone)")
	fs.StringVar(&a.undockInput, "undock-input", "", "`watch`: input on disconnect (empty = leave alone)")
	fs.IntVar(&a.undockVol, "undock-vol", -1, "`watch`: monitor volume on disconnect (-1 = leave alone)")
	fs.BoolVar(&a.undockPower, "undock-power", false, "`watch`: also power the monitor off on disconnect")

	fs.IntVar(&a.step, "step", 5, "`volumekeys`: how much each key press moves 0x62")
	fs.BoolVar(&a.pinWindows, "pin-windows", true, "`volumekeys`: hold the Windows endpoint at 100%")
	fs.DurationVar(&a.coalesce, "coalesce", 20*time.Millisecond,
		"`volumekeys`: minimum interval between DDC writes while a key is held")
	fs.StringVar(&a.audioMatch, "audio-match", "ULTRAGEAR",
		"`volumekeys`: only redirect while the playback device name contains this")
	fs.DurationVar(&a.audioPoll, "audio-poll", 2*time.Second,
		"`volumekeys`: how often to check the default playback device")
}
