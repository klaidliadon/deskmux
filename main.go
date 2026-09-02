// lginput is a command-line monitor control tool for Windows, built for the
// LG UltraGear 45GX950A.
//
// It speaks two protocols:
//
//   - Standard DDC/CI through dxva2.dll, for brightness, volume, PBP,
//     capabilities and arbitrary VCP reads/writes. Verified working.
//   - NVAPI raw I2C, for input switching, which on this panel lives on
//     proprietary VCP 0xF4 at DDC source address 0x50 and is unreachable
//     through any standard Windows API.
//
// Usage:
//
//	lginput [flags] <command> [args]
//
// Flags must precede the command.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// All human-readable output goes through logf/logln so that -log can tee it
// to a file. Errors and usage still go straight to stderr.
var logOut io.Writer = os.Stdout

func logf(format string, a ...any) { fmt.Fprintf(logOut, format, a...) }
func logln(a ...any)               { fmt.Fprintln(logOut, a...) }

const (
	vcpBrightness byte = 0x10
	vcpContrast   byte = 0x12
	vcpInputStd   byte = 0x60 // standard input select -- advertised but ignored on this panel
	vcpVolume     byte = 0x62
	vcpMute       byte = 0x8D
	vcpPower      byte = 0xD6 // DPMS power mode; caps advertise D6(01 04)
	vcpPBP        byte = 0xD7 // LG proprietary, standard 0x51 address
	vcpInputLG    byte = 0xF4 // LG proprietary, needs 0x50 address

	srcStandard byte = 0x51
	srcLGInput  byte = 0x50
)

var inputNames = []struct {
	name  string
	value uint16
}{
	{"hdmi1", 0x90},
	{"hdmi2", 0x91},
	{"dp", 0xD0},
	{"usb-c", 0xD1},
}

var inputAliases = map[string]string{
	"usbc": "usb-c", "typec": "usb-c", "type-c": "usb-c", "tb": "usb-c",
	"dp1": "dp", "displayport": "dp",
	"dp2":  "usb-c", // DP2 and USB-C share 0xD1 on this panel
	"hdmi": "hdmi1",
}

// This panel's capabilities string advertises only D6(01 04), so standby (02)
// and suspend (03) are not offered -- it is on or hard off.
var powerModes = map[string]uint32{
	"on":  0x01,
	"off": 0x04,
}

var pbpModes = map[string]uint16{
	"off": 0x01,
	"50":  0x05, // 50/50 split
	"66":  0x03, // 66/33 split (experimental per ddcutil wiki)
}

var (
	flagMonitor = flag.Int("m", 0, "monitor index (see `list`)")
	flagDry     = flag.Bool("n", false, "dry run: show what would be sent, send nothing")
	flagVerbose = flag.Bool("v", false, "verbose output")
	flagStd     = flag.Bool("std", false, "`input`: use the standard DDC path (source 0x51) instead of NVAPI 0x50")
	flagSrc     = flag.String("src", "", "override DDC source address for `input`/`raw` (e.g. 0x50)")
	flagFast    = flag.Bool("fast", false, "`input`/`raw`: send only the whole-mask I2C write instead of sweeping every port")
	flagSettle  = flag.Duration("settle", 250*time.Millisecond, "delay before verification read-back")
	flagBus     = flag.Duration("bus", 40*time.Millisecond, "delay between raw I2C writes")
	flagVol     = flag.Int("vol", -1, "`input`: also set monitor volume (0x62) as part of the handoff")

	// watch
	flagMatch = flag.String("match", "VID_1E91,VEN_OWC_TB3,SUBSYS_00191C7A",
		"`watch`: comma-separated device-ID substrings identifying the dock")
	flagPoll        = flag.Duration("poll", 2*time.Second, "`watch`: how often to scan for device changes")
	flagDockInput   = flag.String("dock-input", "usb-c", "`watch`: input to select when the dock connects")
	flagDockVol     = flag.Int("dock-vol", -1, "`watch`: monitor volume when the dock connects (-1 = leave alone)")
	flagUndockInput = flag.String("undock-input", "", "`watch`: input on disconnect (empty = leave to Auto Input Switch)")
	flagUndockVol   = flag.Int("undock-vol", -1, "`watch`: monitor volume when the dock disconnects (-1 = leave alone)")

	flagUndockPower = flag.Bool("undock-power", false,
		"`watch`: also power the monitor off (0xD6=4) when the dock disconnects")

	// volumekeys
	flagStep       = flag.Int("step", 5, "`volumekeys`: how much each key press moves 0x62")
	flagPinWindows = flag.Bool("pin-windows", true, "`volumekeys`: hold the Windows endpoint at 100%")
	flagCoalesce   = flag.Duration("coalesce", 20*time.Millisecond, "`volumekeys`: minimum interval between DDC writes while a key is held")
	flagAudioMatch = flag.String("audio-match", "ULTRAGEAR", "`volumekeys`: only redirect while the playback device name contains this")
	flagAudioPoll  = flag.Duration("audio-poll", 2*time.Second, "`volumekeys`: how often to check the default playback device")

	flagLog = flag.String("log", "", "also append all output to this `file`")
)

func sleepSettle() { time.Sleep(*flagSettle) }
func sleepBus()    { time.Sleep(*flagBus) }

func main() {
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	if *flagLog != "" {
		f, err := os.OpenFile(*flagLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot open log %s: %v\n", *flagLog, err)
			os.Exit(1)
		}
		defer f.Close()
		logOut = io.MultiWriter(os.Stdout, f)
		fmt.Fprintf(f, "\n===== %s  lginput %s =====\n",
			time.Now().Format("2006-01-02 15:04:05"), strings.Join(os.Args[1:], " "))
	}

	if err := run(args[0], args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lginput - monitor control for the LG UltraGear 45GX950A (Windows)

usage: lginput [flags] <command> [args]

commands:
  list                    enumerate monitors and show key register values
  caps                    dump the raw DDC capabilities string
  probe                   read every interesting register and report what is writable
  get <vcp>               read one VCP code            (e.g. get 0x10)
  set <vcp> <value>       write one VCP code, verified (e.g. set 0x62 30)
  brightness [v|+v|-v]    get or set brightness (0x10)
  volume     [v|+v|-v]    get or set volume     (0x62)
  mute | unmute           audio mute            (0x8D)
  input <target>          switch input source via NVAPI sidechannel (0xF4 @ 0x50)
                            targets: hdmi1 hdmi2 dp usb-c
  pbp <off|50|66>         picture-by-picture    (0xD7 @ 0x51)
  power <on|off>          monitor power / DPMS   (0xD6)
  raw <vcp> <value>       hand-built packet over NVAPI raw I2C
  watch                   watch for the dock and apply a profile per machine
  volumekeys              volume keys drive the monitor (0x62), Windows pinned at 100%
  devices [substr]        list present device IDs (to find a -match string)

flags (must precede the command):
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
notes:
  DDC writes are fire-and-forget: success means "bytes sent", not "monitor
  obeyed". Commands that can verify do so by reading back. Input switching
  cannot be verified -- that channel never acknowledges -- so judge it by
  looking at the screen.
`)
}

func run(cmd string, args []string) error {
	switch cmd {
	case "list":
		return cmdList()
	case "caps":
		return cmdCaps()
	case "probe":
		return cmdProbe()
	case "get":
		return cmdGet(args)
	case "set":
		return cmdSet(args)
	case "brightness":
		return cmdLevel(args, vcpBrightness, "brightness")
	case "volume":
		return cmdLevel(args, vcpVolume, "volume")
	case "mute":
		return cmdMute(1)
	case "unmute":
		return cmdMute(2)
	case "input":
		return cmdInput(args)
	case "pbp":
		return cmdPBP(args)
	case "power":
		return cmdPower(args)
	case "raw":
		return cmdRaw(args)
	case "watch":
		return cmdWatch(args)
	case "volumekeys":
		return cmdVolumeKeys(args)
	case "devices":
		return cmdDevices(args)
	default:
		return fmt.Errorf("unknown command %q (run with no args for usage)", cmd)
	}
}

// pickMonitor opens the monitor set and returns the selected monitor.
// Caller must Close the returned set.
func pickMonitor() (*Monitors, Monitor, error) {
	ms, err := OpenMonitors()
	if err != nil {
		return nil, Monitor{}, err
	}
	if *flagMonitor < 0 || *flagMonitor >= len(ms.List) {
		ms.Close()
		return nil, Monitor{}, fmt.Errorf("monitor index %d out of range (%d found)", *flagMonitor, len(ms.List))
	}
	return ms, ms.List[*flagMonitor], nil
}

func cmdList() error {
	ms, err := OpenMonitors()
	if err != nil {
		return err
	}
	defer ms.Close()

	for i, m := range ms.List {
		logf("[%d] %s\n", i, m.Name)
		for _, r := range []struct {
			code byte
			name string
		}{
			{vcpBrightness, "brightness"},
			{vcpContrast, "contrast"},
			{vcpVolume, "volume"},
			{vcpMute, "mute"},
			{vcpInputStd, "input (0x60, unreliable)"},
		} {
			v, err := m.GetVCP(r.code)
			if err != nil {
				logf("     0x%02X %-26s unsupported\n", r.code, r.name)
				continue
			}
			logf("     0x%02X %-26s %d (max %d)\n", r.code, r.name, v.Current, v.Max)
		}
	}
	return nil
}

func cmdCaps() error {
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()
	caps, err := m.Capabilities()
	if err != nil {
		return err
	}
	logln(caps)
	return nil
}

func cmdProbe() error {
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()

	logf("monitor: %s\n\n", m.Name)
	caps, err := m.Capabilities()
	if err == nil {
		logf("caps: %s\n\n", caps)
	}

	codes := []struct {
		code byte
		name string
	}{
		{vcpBrightness, "brightness"},
		{vcpContrast, "contrast"},
		{vcpVolume, "volume"},
		{vcpMute, "mute"},
		{vcpInputStd, "input select (standard)"},
		{vcpInputLG, "input select (LG proprietary)"},
		{vcpPBP, "PBP (LG proprietary)"},
	}
	logf("%-6s %-28s %s\n", "code", "name", "read")
	for _, c := range codes {
		v, err := m.GetVCP(c.code)
		if err != nil {
			logf("0x%02X   %-28s unsupported\n", c.code, c.name)
			continue
		}
		logf("0x%02X   %-28s current=%d max=%d type=%d\n", c.code, c.name, v.Current, v.Max, v.Type)
	}
	logln("\nnote: a register reading fine says nothing about whether writes land.")
	logln("      On the 45GX950A, 0x60 reads and is silently ignored on write.")
	return nil
}

func parseVCP(s string) (byte, error) {
	n, err := strconv.ParseUint(s, 0, 8)
	if err != nil {
		return 0, fmt.Errorf("bad VCP code %q: %w", s, err)
	}
	return byte(n), nil
}

func parseValue(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("bad value %q: %w", s, err)
	}
	return uint32(n), nil
}

func cmdGet(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: get <vcp>")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()
	v, err := m.GetVCP(code)
	if err != nil {
		return err
	}
	logf("0x%02X: current=%d (0x%02X) max=%d type=%d\n", code, v.Current, v.Current, v.Max, v.Type)
	return nil
}

func cmdSet(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: set <vcp> <value>")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}
	val, err := parseValue(args[1])
	if err != nil {
		return err
	}
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()

	if *flagDry {
		logf("dry-run: SetVCPFeature(0x%02X, %d) via dxva2 (source 0x51)\n", code, val)
		return nil
	}
	landed, got, err := m.SetVCPVerified(code, val)
	if err != nil {
		return err
	}
	logf("0x%02X <- %d; read back %d; landed=%v\n", code, val, got, landed)
	if !landed {
		logln("write was accepted by the API but the monitor did not take it")
	}
	return nil
}

func cmdLevel(args []string, code byte, label string) error {
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()

	cur, err := m.GetVCP(code)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		logf("%s: %d (max %d)\n", label, cur.Current, cur.Max)
		return nil
	}

	target, err := resolveLevel(args[0], cur)
	if err != nil {
		return err
	}
	if *flagDry {
		logf("dry-run: %s %d -> %d\n", label, cur.Current, target)
		return nil
	}
	landed, got, err := m.SetVCPVerified(code, target)
	if err != nil {
		return err
	}
	logf("%s: %d -> %d (read back %d, landed=%v)\n", label, cur.Current, target, got, landed)
	return nil
}

func resolveLevel(arg string, cur VCPValue) (uint32, error) {
	rel := strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "-")
	n, err := strconv.ParseInt(arg, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad level %q (want N, +N or -N)", arg)
	}
	v := n
	if rel {
		v = int64(cur.Current) + n
	}
	if v < 0 {
		v = 0
	}
	if v > int64(cur.Max) {
		v = int64(cur.Max)
	}
	return uint32(v), nil
}

func cmdMute(v uint32) error {
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()
	if *flagDry {
		logf("dry-run: SetVCPFeature(0x8D, %d)\n", v)
		return nil
	}
	landed, got, err := m.SetVCPVerified(vcpMute, v)
	if err != nil {
		return err
	}
	logf("mute 0x8D <- %d; read back %d; landed=%v\n", v, got, landed)
	return nil
}

func resolveInput(name string) (uint16, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if alias, ok := inputAliases[n]; ok {
		n = alias
	}
	for _, in := range inputNames {
		if in.name == n {
			return in.value, nil
		}
	}
	var valid []string
	for _, in := range inputNames {
		valid = append(valid, in.name)
	}
	sort.Strings(valid)
	return 0, fmt.Errorf("unknown input %q (want one of: %s)", name, strings.Join(valid, ", "))
}

func cmdInput(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: input <hdmi1|hdmi2|dp|usb-c>")
	}
	val, err := resolveInput(args[0])
	if err != nil {
		return err
	}

	// --std sends VCP 0xF4 through the ordinary Windows API, which forces
	// source address 0x51. Documented not to work for input on this family,
	// but it costs nothing to try and would mean no NVAPI is needed at all.
	if *flagStd {
		ms, m, err := pickMonitor()
		if err != nil {
			return err
		}
		defer ms.Close()
		logf("standard DDC path (source 0x51): SetVCPFeature(0x%02X, 0x%02X)\n", vcpInputLG, val)
		if *flagDry {
			return nil
		}
		if err := m.SetVCP(vcpInputLG, uint32(val)); err != nil {
			return err
		}
		logln("sent. This channel does not acknowledge -- check the screen.")
		return nil
	}

	src := srcLGInput
	if *flagSrc != "" {
		s, err := parseVCP(*flagSrc)
		if err != nil {
			return err
		}
		src = s
	}

	// Volume handoff.
	//
	// 0x62 is a monitor-global setting shared by every input, and the two
	// machines want opposite things: Windows can attenuate digitally so it
	// wants 0x62 high, while macOS cannot attenuate DP/HDMI display audio at
	// all, so over there 0x62 IS the volume control. Whoever wrote it last
	// wins, which is how you end up handing the Mac a monitor at maximum.
	//
	// Setting it as part of the switch makes the level deterministic no
	// matter what the other machine did. We try before AND after: before
	// covers switching away (the DDC link drops once the panel leaves us),
	// after covers switching to ourselves (DDC may not be reachable until
	// the panel is actually showing us). The write is idempotent, so doing
	// both is harmless and one of them will land.
	if *flagVol >= 0 && !*flagDry {
		logf("  volume (pre):  %s\n", setVolumeBestEffort(uint32(*flagVol)))
	} else if *flagVol >= 0 {
		logf("  volume (pre):  dry-run, would set 0x62 = %d\n", *flagVol)
	}

	err = sendRaw(src, vcpInputLG, val, fmt.Sprintf("input -> %s", args[0]))

	if *flagVol >= 0 && !*flagDry {
		time.Sleep(2 * time.Second)
		logf("  volume (post): %s\n", setVolumeBestEffort(uint32(*flagVol)))
	}
	return err
}

// setVolumeBestEffort writes VCP 0x62, preferring the verifiable DDC path and
// falling back to NVAPI raw I2C when the Windows DDC layer is unavailable
// (which happens routinely on this panel -- see the wedging note in ddc.go).
func setVolumeBestEffort(v uint32) string {
	if ms, m, err := pickMonitor(); err == nil {
		defer ms.Close()
		if landed, got, err := m.SetVCPVerified(vcpVolume, v); err == nil {
			if landed {
				return fmt.Sprintf("%d via DDC, verified", got)
			}
			return fmt.Sprintf("sent %d via DDC but read back %d", v, got)
		}
	}
	nv, err := loadNVAPI()
	if err != nil {
		return fmt.Sprintf("DDC unavailable and NVAPI failed: %v", err)
	}
	ok := 0
	attempts := nv.WritePacket(BuildSetVCP(srcStandard, vcpVolume, uint16(v)), false, false)
	for _, a := range attempts {
		if a.OK {
			ok++
		}
	}
	return fmt.Sprintf("%d over NVAPI raw, %d/%d accepted, unverified", v, ok, len(attempts))
}

// cmdPower drives DPMS via VCP 0xD6. Powering off is one-way from our side:
// once the panel is off it stops answering DDC, so `power on` will usually
// fail and the monitor has to be woken by a signal or the joystick.
func cmdPower(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: power <on|off>")
	}
	mode, ok := powerModes[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("unknown power mode %q (want on or off)", args[0])
	}
	logf("power %s: SetVCPFeature(0x%02X, 0x%02X)\n", args[0], vcpPower, mode)
	if *flagDry {
		return nil
	}
	if ms, m, err := pickMonitor(); err == nil {
		defer ms.Close()
		if err := m.SetVCP(vcpPower, mode); err == nil {
			logln("  sent via DDC")
			return nil
		}
	}
	// DDC layer unavailable -- fall back to the raw bus.
	return sendRaw(srcStandard, vcpPower, uint16(mode), fmt.Sprintf("power %s", args[0]))
}

func cmdPBP(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: pbp <off|50|66>")
	}
	mode, ok := pbpModes[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("unknown pbp mode %q (want off, 50 or 66)", args[0])
	}
	// PBP is documented to work at the standard address, so use the normal API.
	ms, m, err := pickMonitor()
	if err != nil {
		return err
	}
	defer ms.Close()
	logf("pbp %s: SetVCPFeature(0x%02X, 0x%02X) via dxva2 (source 0x51)\n", args[0], vcpPBP, mode)
	if *flagDry {
		return nil
	}
	if err := m.SetVCP(vcpPBP, uint32(mode)); err != nil {
		return err
	}
	sleepSettle()
	if v, err := m.GetVCP(vcpPBP); err == nil {
		logf("read back: %d (0x%02X)\n", v.Current, v.Current)
	}
	return nil
}

func cmdRaw(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: raw <vcp> <value>   (use -src to set the source address)")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}
	val, err := parseValue(args[1])
	if err != nil {
		return err
	}
	src := srcStandard
	if *flagSrc != "" {
		s, err := parseVCP(*flagSrc)
		if err != nil {
			return err
		}
		src = s
	}
	return sendRaw(src, code, uint16(val), "raw")
}

// sendRaw builds the packet and pushes it over NVAPI raw I2C.
func sendRaw(src, vcp byte, value uint16, label string) error {
	pkt := BuildSetVCP(src, vcp, value)

	var hex []string
	for _, b := range pkt {
		hex = append(hex, fmt.Sprintf("%02X", b))
	}
	logf("%s\n", label)
	logf("  packet: %s   (src=0x%02X vcp=0x%02X value=0x%04X, checksum over dest 0x6E)\n",
		strings.Join(hex, " "), src, vcp, value)

	if *flagDry {
		logln("  dry-run: nothing sent")
		return nil
	}

	nv, err := loadNVAPI()
	if err != nil {
		return err
	}
	attempts := nv.WritePacket(pkt, false, *flagVerbose)

	var ok int
	for _, a := range attempts {
		if a.OK {
			ok++
		}
		if *flagVerbose {
			port := "-"
			if a.HasPort {
				port = strconv.Itoa(a.Port)
			}
			logf("    gpu=%d mask=0x%08X port=%s ok=%v %s\n", a.GPU, a.Mask, port, a.OK, a.Status)
		}
	}
	logf("  %d/%d writes accepted by the bus\n", ok, len(attempts))
	if ok == 0 {
		return fmt.Errorf("every I2C write was rejected; is the monitor on the NVIDIA GPU?")
	}
	logln("  this channel never acknowledges -- confirm by looking at the screen")
	return nil
}
