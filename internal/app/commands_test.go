package app

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klaidliadon/deskmux/config"
	"github.com/klaidliadon/deskmux/ddc"
	"github.com/klaidliadon/deskmux/vcp"
)

// newFakeApp builds an App whose hardware seams are fakes, so the command
// layer runs end to end with no monitor attached. Everything below this line
// was untestable before Panel and Bus existed.
func newFakeApp(t *testing.T, opts Options) (*App, *fakeOpener, *fakeBus, *bytes.Buffer) {
	t.Helper()

	var out bytes.Buffer
	a := New(config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)), &out, opts)

	opener := &fakeOpener{panel: newFakePanel()}
	bus := newFakeBus()
	a.panels, a.bus = opener, bus

	// Never synthesise real input events from a test.
	a.wake = func() (time.Duration, error) { return 0, nil }

	return a, opener, bus, &out
}

// countingWaker records calls and can be made to fail.
func countingWaker(n *int, err error) func() (time.Duration, error) {
	return func() (time.Duration, error) {
		*n++
		return 90 * time.Second, err
	}
}

func liveOpts() Options { return Options{Monitor: -1} }

// --- input -------------------------------------------------------------

// The bytes are the ones verified against a real LG 45GX950A. Dry runs already
// pinned the printed packet; this pins what actually reaches the bus.
func TestInputSendsTheVerifiedPacket(t *testing.T) {
	tests := []struct {
		input string
		want  []byte
	}{
		{"usb-c", []byte{0x50, 0x84, 0x03, 0xF4, 0x00, 0xD1, 0x9C}},
		{"dp", []byte{0x50, 0x84, 0x03, 0xF4, 0x00, 0xD0, 0x9D}},
		{"hdmi1", []byte{0x50, 0x84, 0x03, 0xF4, 0x00, 0x90, 0xDD}},
		{"hdmi2", []byte{0x50, 0x84, 0x03, 0xF4, 0x00, 0x91, 0xDC}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			a, opener, bus, _ := newFakeApp(t, liveOpts())

			if err := a.Input([]string{tt.input}); err != nil {
				t.Fatalf("Input(%q): %v", tt.input, err)
			}
			if got := bus.lastPacket(t); !bytes.Equal(got, tt.want) {
				t.Errorf("packet = % X, want % X", got, tt.want)
			}
			// Input never needs the DDC layer: that is the whole point of the
			// sidechannel, and opening a monitor is itself a bus transaction.
			if opener.opens != 0 {
				t.Errorf("input opened the DDC layer %d times, want 0", opener.opens)
			}
		})
	}
}

// -fast must reach the bus, or the flag silently does nothing.
func TestInputPropagatesFast(t *testing.T) {
	for _, fast := range []bool{false, true} {
		a, _, bus, _ := newFakeApp(t, Options{Monitor: -1, Fast: fast})

		if err := a.Input([]string{"dp"}); err != nil {
			t.Fatal(err)
		}
		if got := bus.opts[0].Fast; got != fast {
			t.Errorf("Fast = %v, want %v", got, fast)
		}
		if got, want := bus.opts[0].Delay, config.Default().DDC.BusDelay.D(); got != want {
			t.Errorf("Delay = %s, want the configured %s", got, want)
		}
	}
}

// A monitor on a non-NVIDIA output takes nothing. Reporting success there
// would be a lie the user could only catch by looking at the screen.
func TestInputFailsWhenTheBusAcceptsNothing(t *testing.T) {
	a, _, bus, out := newFakeApp(t, liveOpts())
	bus.accepted, bus.total = 0, 19

	err := a.Input([]string{"dp"})
	if err == nil {
		t.Fatal("expected an error when every write was rejected")
	}
	if !strings.Contains(err.Error(), "NVIDIA") {
		t.Errorf("error %q should point at the likely cause", err)
	}
	if strings.Contains(out.String(), "confirm by looking at the screen") {
		t.Error("a total failure should not invite the user to check the screen")
	}
}

// A partial acceptance is the normal case on the development panel: the
// combined-mask write is always rejected, per-output-bit writes land.
func TestInputSucceedsOnPartialAcceptance(t *testing.T) {
	a, _, bus, out := newFakeApp(t, liveOpts())
	bus.accepted, bus.total = 18, 19

	if err := a.Input([]string{"usb-c"}); err != nil {
		t.Fatalf("18 of 19 accepted should not be an error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "18/19 writes accepted") {
		t.Errorf("output should report the tally:\n%s", got)
	}
}

// Nothing attempted is not the same as everything rejected. Observed live: a
// hybrid laptop parked its discrete GPU after hours idle and the mux handed
// the panel to the integrated one, so NVAPI reported no connected outputs at
// all while the display stayed lit and working.
func TestInputDistinguishesNoBusFromRejection(t *testing.T) {
	t.Run("no connected outputs", func(t *testing.T) {
		a, _, bus, out := newFakeApp(t, liveOpts())
		bus.accepted, bus.total = 0, 0

		err := a.Input([]string{"dp"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no bus to write to") {
			t.Errorf("error %q should say there was no bus", err)
		}
		if strings.Contains(err.Error(), "rejected") {
			t.Errorf("error %q blames rejection, but nothing was attempted", err)
		}
		if got := out.String(); !strings.Contains(got, "0/0 writes accepted") {
			t.Errorf("output should show 0/0:\n%s", got)
		}
	})

	t.Run("attempted and rejected", func(t *testing.T) {
		a, _, bus, _ := newFakeApp(t, liveOpts())
		bus.accepted, bus.total = 0, 19

		err := a.Input([]string{"dp"})
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Errorf("error %q should say the writes were rejected", err)
		}
	})
}

func TestInputReportsBusFailure(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, liveOpts())
	bus.err = errors.New("load nvapi64.dll: not found")

	if err := a.Input([]string{"dp"}); err == nil {
		t.Fatal("a bus that cannot load should fail the command")
	}
}

func TestInputDryRunSendsNothing(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	if err := a.Input([]string{"dp"}); err != nil {
		t.Fatal(err)
	}
	if len(bus.packets) != 0 {
		t.Errorf("dry run put %d packets on the bus", len(bus.packets))
	}
}

// --- set / get ---------------------------------------------------------

// The behaviour this whole tool exists because of: the write is accepted and
// the register does not change. `set` must say so rather than report success.
func TestSetReportsARegisterThatIgnoresWrites(t *testing.T) {
	a, opener, _, out := newFakeApp(t, liveOpts())
	opener.panel.readings[0x60] = ddc.Reading{Current: 15, Max: 17}
	opener.panel.ignored[0x60] = true

	if err := a.Set([]string{"0x60", "17"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "landed=false") {
		t.Errorf("output should report landed=false:\n%s", got)
	}
	if !strings.Contains(got, "did not take it") {
		t.Errorf("output should explain the failure in words:\n%s", got)
	}
}

func TestSetReportsARegisterThatTakesWrites(t *testing.T) {
	a, opener, _, out := newFakeApp(t, liveOpts())

	if err := a.Set([]string{"0x10", "42"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "landed=true") {
		t.Errorf("output should report landed=true:\n%s", got)
	}
	if w := opener.panel.lastWrite(t); w.Code != 0x10 || w.Value != 42 {
		t.Errorf("wrote %s = %s, want 0x10 = 42", w.Code, w.Value)
	}
}

// A dry run must be usable with no monitor attached, so it cannot open one.
func TestSetDryRunNeverOpensTheMonitor(t *testing.T) {
	a, opener, _, out := newFakeApp(t, Options{Monitor: -1, DryRun: true})
	opener.err = ddc.ErrNoMonitors

	if err := a.Set([]string{"0x10", "42"}); err != nil {
		t.Fatalf("dry run failed with no monitor attached: %v", err)
	}
	if opener.opens != 0 {
		t.Error("dry run opened the monitor")
	}
	if got := out.String(); !strings.Contains(got, "dry-run: set 0x10 = 42") {
		t.Errorf("unexpected output: %s", got)
	}
}

func TestGetFormatsTheReading(t *testing.T) {
	a, _, _, out := newFakeApp(t, liveOpts())

	if err := a.Get([]string{"0x62"}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The %02X-on-a-Stringer bug printed 0x30783632 here.
	if want := "0x62: current=27 (0x1B) max=100 (0x64)"; !strings.Contains(out.String(), want) {
		t.Errorf("got %q, want it to contain %q", out.String(), want)
	}
}

func TestGetPropagatesAnUnsupportedRegister(t *testing.T) {
	a, _, _, _ := newFakeApp(t, liveOpts())

	if err := a.Get([]string{"0xFE"}); err == nil {
		t.Fatal("reading an unsupported register should fail")
	}
}

// --- level -------------------------------------------------------------

func TestLevelWritesTheResolvedTarget(t *testing.T) {
	tests := []struct {
		arg  string
		want vcp.Level
	}{
		{"70", 70},
		{"+10", 37}, // the fake panel starts at 27
		{"-10", 17},
		{"+999", 100},
		{"-999", 0},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			a, opener, _, _ := newFakeApp(t, liveOpts())

			if err := a.Level([]string{tt.arg}, 0x62, "volume"); err != nil {
				t.Fatalf("Level(%q): %v", tt.arg, err)
			}
			if w := opener.panel.lastWrite(t); w.Value != tt.want {
				t.Errorf("wrote %s, want %s", w.Value, tt.want)
			}
		})
	}
}

func TestLevelWithNoArgumentOnlyReads(t *testing.T) {
	a, opener, _, out := newFakeApp(t, liveOpts())

	if err := a.Level(nil, 0x62, "volume"); err != nil {
		t.Fatalf("Level: %v", err)
	}
	if len(opener.panel.writes) != 0 {
		t.Error("reading the level wrote to the monitor")
	}
	if want := "volume: 27 (max 100)"; !strings.Contains(out.String(), want) {
		t.Errorf("got %q, want it to contain %q", out.String(), want)
	}
}

func TestLevelDryRunWritesNothing(t *testing.T) {
	a, opener, _, out := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	if err := a.Level([]string{"+10"}, 0x62, "volume"); err != nil {
		t.Fatalf("Level: %v", err)
	}
	if len(opener.panel.writes) != 0 {
		t.Errorf("dry run wrote %v", opener.panel.writes)
	}
	// It must still resolve the relative argument against the live reading,
	// which is the reason this one opens the monitor at all.
	if want := "dry-run: volume 27 -> 37"; !strings.Contains(out.String(), want) {
		t.Errorf("got %q, want it to contain %q", out.String(), want)
	}
}

// --- mute --------------------------------------------------------------

func TestMuteWritesTheMCCSValues(t *testing.T) {
	tests := []struct {
		name  string
		value vcp.Level
	}{
		{"mute", _muteOn},
		{"unmute", _muteOff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, opener, _, _ := newFakeApp(t, liveOpts())

			if err := a.Mute(tt.value); err != nil {
				t.Fatalf("Mute: %v", err)
			}
			w := opener.panel.lastWrite(t)
			if w.Code != config.Default().Registers.Mute || w.Value != tt.value {
				t.Errorf("wrote %s = %s, want 0x8D = %s", w.Code, w.Value, tt.value)
			}
		})
	}
}

func TestMuteDryRunNeverOpensTheMonitor(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})
	opener.err = ddc.ErrNoMonitors

	if err := a.Mute(_muteOn); err != nil {
		t.Fatalf("dry run failed with no monitor: %v", err)
	}
	if opener.opens != 0 {
		t.Error("dry run opened the monitor")
	}
}

// --- table (pbp / power) -----------------------------------------------

// Both answer at the standard address, so DDC is tried first and the raw path
// is a fallback, not the norm.
func TestTablePrefersDDC(t *testing.T) {
	a, opener, bus, out := newFakeApp(t, liveOpts())

	if err := a.Table([]string{"off"}, config.Default().PBP, "pbp"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	if w := opener.panel.lastWrite(t); w.Code != 0xD7 || w.Value != 0x01 {
		t.Errorf("wrote %s = %s, want 0xD7 = 1", w.Code, w.Value)
	}
	if len(bus.packets) != 0 {
		t.Error("the raw bus was used even though DDC succeeded")
	}
	if !strings.Contains(out.String(), "sent via DDC") {
		t.Errorf("output should say which path was taken:\n%s", out.String())
	}
}

// The documented recovery path: once the panel's DDC engine wedges, `power
// off` has to go out over raw I2C. It had never been exercised.
func TestTableFallsBackToRawWhenDDCWriteFails(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())
	opener.panel.setErr[0xD6] = errors.New("error receiving data from the device on the I2C bus")

	if err := a.Table([]string{"off"}, config.Default().Power, "power"); err != nil {
		t.Fatalf("Table: %v", err)
	}

	want := []byte{0x51, 0x84, 0x03, 0xD6, 0x00, 0x04, 0x6A}
	if got := bus.lastPacket(t); !bytes.Equal(got, want) {
		t.Errorf("packet = % X, want % X", got, want)
	}
}

// When the DDC layer will not open at all -- which is what a wedged engine
// actually looks like from Windows -- the fallback must still fire.
func TestTableFallsBackToRawWhenTheMonitorWillNotOpen(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())
	opener.err = ddc.ErrNoMonitors

	if err := a.Table([]string{"off"}, config.Default().Power, "power"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	if len(bus.packets) != 1 {
		t.Fatalf("bus saw %d packets, want 1", len(bus.packets))
	}
}

func TestTableDryRunTouchesNothing(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	if err := a.Table([]string{"off"}, config.Default().Power, "power"); err != nil {
		t.Fatalf("Table: %v", err)
	}
	if opener.opens != 0 || len(bus.packets) != 0 {
		t.Errorf("dry run touched hardware: opens=%d packets=%d", opener.opens, len(bus.packets))
	}
}

// --- probe -------------------------------------------------------------

// Probe reads eight registers on a panel that supports some of them. A single
// unsupported register must not abort the report.
func TestProbeToleratesUnsupportedRegistersAndCapabilities(t *testing.T) {
	a, opener, _, out := newFakeApp(t, liveOpts())
	opener.panel.capsErr = errors.New("GetCapabilitiesStringLength failed")

	if err := a.Probe(); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// Probe pads into columns, so compare on collapsed whitespace rather than
	// pinning a column width that is presentation, not behaviour.
	got := out.String()
	collapsed := strings.Join(strings.Fields(got), " ")

	for _, want := range []string{
		"monitor: FAKE PANEL",
		"0x10 brightness current=100 max=100",
		"0x62 volume current=27 max=100",
		"0x60 input select (standard) unsupported", // absent from the fake
		"0xF4 input select (configured) unsupported",
	} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("probe output missing %q:\n%s", want, got)
		}
	}
	if opener.panel.closes == 0 {
		t.Error("probe leaked the monitor handle")
	}
}

// --- handle lifetime ---------------------------------------------------

// Every command that opens a panel must close it. A leak here is invisible
// until a long-lived daemon exhausts the handles.
func TestCommandsCloseWhatTheyOpen(t *testing.T) {
	cfg := config.Default()

	tests := []struct {
		name string
		call func(a *App) error
	}{
		{"probe", func(a *App) error { return a.Probe() }},
		{"get", func(a *App) error { return a.Get([]string{"0x62"}) }},
		{"set", func(a *App) error { return a.Set([]string{"0x10", "42"}) }},
		{"level", func(a *App) error { return a.Level([]string{"50"}, 0x62, "volume") }},
		{"mute", func(a *App) error { return a.Mute(_muteOn) }},
		{"table", func(a *App) error { return a.Table([]string{"off"}, cfg.PBP, "pbp") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, opener, _, _ := newFakeApp(t, liveOpts())

			if err := tt.call(a); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if opener.opens != opener.panel.closes {
				t.Errorf("opened %d, closed %d", opener.opens, opener.panel.closes)
			}
		})
	}
}

func TestOpenPanelUsesTheResolvedIndex(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, Options{Monitor: 2})

	if err := a.Get([]string{"0x62"}); err != nil {
		t.Fatal(err)
	}
	if got := opener.indexes; !slices.Equal(got, []int{2}) {
		t.Errorf("opened indexes %v, want [2]", got)
	}
}

// --- setVolume ---------------------------------------------------------

func TestSetVolumePrefersVerifiedDDC(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, liveOpts())

	got := a.setVolume(40)
	if want := "40 via DDC, verified"; got != want {
		t.Errorf("setVolume = %q, want %q", got, want)
	}
	if len(bus.packets) != 0 {
		t.Error("the raw bus was used even though DDC verified")
	}
}

func TestSetVolumeReportsAnUnverifiedDDCWrite(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, liveOpts())
	opener.panel.ignored[0x62] = true

	if got, want := a.setVolume(40), "sent 40 via DDC but read back 27"; got != want {
		t.Errorf("setVolume = %q, want %q", got, want)
	}
}

func TestSetVolumeFallsBackToRaw(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())
	opener.err = ddc.ErrNoMonitors
	bus.accepted, bus.total = 1, 1

	got := a.setVolume(40)
	if want := "40 over NVAPI raw, 1/1 accepted, unverified"; got != want {
		t.Errorf("setVolume = %q, want %q", got, want)
	}

	want := []byte{0x51, 0x84, 0x03, 0x62, 0x00, 0x28, 0xF2}
	if pkt := bus.lastPacket(t); !bytes.Equal(pkt, want) {
		t.Errorf("packet = % X, want % X", pkt, want)
	}
}

func TestSetVolumeReportsBothPathsFailing(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())
	opener.err = ddc.ErrNoMonitors
	bus.err = errors.New("no NVIDIA GPUs found")

	if got := a.setVolume(40); !strings.Contains(got, "NVAPI failed") {
		t.Errorf("setVolume = %q, want it to name both failures", got)
	}
}

// A dry run reaches setVolume through applyProfile, which is how `-n` used to
// write the monitor for real.
func TestSetVolumeDryRunWritesNothing(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	if got := a.setVolume(40); !strings.Contains(got, "dry-run") {
		t.Errorf("setVolume = %q, want it to say dry-run", got)
	}
	if opener.opens != 0 || len(bus.packets) != 0 {
		t.Errorf("dry run touched hardware: opens=%d packets=%d", opener.opens, len(bus.packets))
	}
}

// --- applyProfile ------------------------------------------------------

func TestApplyProfileAppliesVolumeThenInput(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())

	a.applyProfile(config.Profile{Input: "usb-c", Volume: 30}, "dock")

	if w := opener.panel.lastWrite(t); w.Code != 0x62 || w.Value != 30 {
		t.Errorf("volume write = %s = %s, want 0x62 = 30", w.Code, w.Value)
	}
	want := []byte{0x50, 0x84, 0x03, 0xF4, 0x00, 0xD1, 0x9C}
	if got := bus.lastPacket(t); !bytes.Equal(got, want) {
		t.Errorf("input packet = % X, want % X", got, want)
	}
}

// Volume -1 means "leave it alone", which is the shipped default. Writing 0
// here would mute the panel on every dock transition.
func TestApplyProfileSkipsNegativeVolume(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, liveOpts())

	a.applyProfile(config.Profile{Input: "dp", Volume: -1}, "undock")

	if len(opener.panel.writes) != 0 {
		t.Errorf("volume -1 still wrote %v", opener.panel.writes)
	}
}

func TestApplyProfileSkipsEmptyInput(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, liveOpts())

	a.applyProfile(config.Profile{Volume: 30}, "dock")

	if len(bus.packets) != 0 {
		t.Error("an empty input still switched the monitor")
	}
}

// One failing step must not abort the rest: a dock arriving must still get the
// input switch even if the volume write failed.
func TestApplyProfileIsBestEffort(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())
	opener.panel.setErr[0x62] = errors.New("bus error")

	a.applyProfile(config.Profile{Input: "usb-c", Volume: 30}, "dock")

	if len(bus.packets) == 0 {
		t.Fatal("a failed volume write suppressed the input switch")
	}
}

// Power goes last, because the panel stops answering DDC once it is off.
func TestApplyProfilePowersOffLast(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, liveOpts())

	a.applyProfile(config.Profile{Input: "dp", Volume: 30, PowerOff: true}, "undock")

	codes := make([]vcp.Code, 0, len(opener.panel.writes))
	for _, w := range opener.panel.writes {
		codes = append(codes, w.Code)
	}
	if !slices.Equal(codes, []vcp.Code{0x62, 0xD6}) {
		t.Errorf("write order = %v, want volume (0x62) then power (0xD6)", codes)
	}
	if len(bus.packets) != 1 {
		t.Errorf("bus saw %d packets, want just the input switch", len(bus.packets))
	}
}

func TestApplyProfileDryRunTouchesNothing(t *testing.T) {
	a, opener, bus, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	a.applyProfile(config.Profile{Input: "usb-c", Volume: 30, PowerOff: true}, "dock")

	if opener.opens != 0 {
		t.Errorf("dry run opened the monitor %d times", opener.opens)
	}
	if len(bus.packets) != 0 {
		t.Errorf("dry run put %d packets on the bus", len(bus.packets))
	}
}

// --- wake --------------------------------------------------------------

// The panel stays lit on the other machine while Windows blanks its own
// output, so claiming the panel without waking lands on a dead input. This is
// the step that makes a dock arriving after the display timeout work at all.
func TestApplyProfileWakesBeforeSwitchingInput(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, liveOpts())

	var wakes int
	a.wake = func() (time.Duration, error) {
		wakes++
		if len(bus.packets) != 0 {
			t.Error("the input switch went out before the display was woken")
		}
		return 90 * time.Second, nil
	}

	a.applyProfile(config.Profile{Input: "usb-c", Volume: -1, Wake: true}, "dock")

	if wakes != 1 {
		t.Errorf("wakes = %d, want 1", wakes)
	}
	if len(bus.packets) != 1 {
		t.Errorf("bus saw %d packets, want the input switch", len(bus.packets))
	}
}

// Handing the panel away must not keep this machine awake.
func TestApplyProfileDoesNotWakeWhenNotAsked(t *testing.T) {
	a, _, _, _ := newFakeApp(t, liveOpts())

	var wakes int
	a.wake = countingWaker(&wakes, nil)

	a.applyProfile(config.Profile{Input: "dp", Volume: -1}, "undock")

	if wakes != 0 {
		t.Errorf("wakes = %d, want 0 for a profile that does not ask", wakes)
	}
}

// A wake that fails must not suppress the handover: the input switch is still
// worth making and the user can nudge the mouse themselves.
func TestApplyProfileSwitchesInputEvenIfWakeFails(t *testing.T) {
	a, _, bus, _ := newFakeApp(t, liveOpts())
	a.wake = countingWaker(new(int), errors.New("GetLastInputInfo failed"))

	a.applyProfile(config.Profile{Input: "usb-c", Volume: -1, Wake: true}, "dock")

	if len(bus.packets) != 1 {
		t.Error("a failed wake suppressed the input switch")
	}
}

func TestApplyProfileDryRunDoesNotWake(t *testing.T) {
	a, _, _, _ := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	var wakes int
	a.wake = countingWaker(&wakes, nil)

	a.applyProfile(config.Profile{Input: "usb-c", Volume: -1, Wake: true}, "dock")

	if wakes != 0 {
		t.Errorf("dry run woke the display %d times", wakes)
	}
}

func TestWakeCommand(t *testing.T) {
	a, _, _, out := newFakeApp(t, liveOpts())

	var wakes int
	a.wake = countingWaker(&wakes, nil)

	if err := a.Wake(); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1", wakes)
	}
	if want := "display woken (idle for 1m30s)"; !strings.Contains(out.String(), want) {
		t.Errorf("got %q, want it to contain %q", out.String(), want)
	}
}

func TestWakeCommandDryRun(t *testing.T) {
	a, _, _, out := newFakeApp(t, Options{Monitor: -1, DryRun: true})

	var wakes int
	a.wake = countingWaker(&wakes, nil)

	if err := a.Wake(); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if wakes != 0 {
		t.Error("dry run woke the display")
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestWakeCommandPropagatesFailure(t *testing.T) {
	a, _, _, _ := newFakeApp(t, liveOpts())
	a.wake = countingWaker(new(int), errors.New("GetLastInputInfo failed"))

	if err := a.Wake(); err == nil {
		t.Fatal("a failing wake should surface as an error from the command")
	}
}

// idleTime talks to the real OS. It cannot assert a value, but it can assert
// that the call succeeds and returns something sane, which is what catches a
// wrong struct size or a missing export.
func TestIdleTimeIsPlausible(t *testing.T) {
	idle, err := idleTime()
	if err != nil {
		t.Fatalf("idleTime: %v", err)
	}
	if idle < 0 || idle > 49*24*time.Hour {
		t.Errorf("idle = %s, outside the range GetTickCount can express", idle)
	}
}

// --- volumeSession -----------------------------------------------------

// The panel's DDC engine drops out and comes back. A write that fails must
// cost one reopen, not a permanently dead session.
func TestVolumeSessionReopensOnceOnAStaleHandle(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, liveOpts())
	opener.panel.failWritesUntilReopen = true
	opener.onOpen = func(p *fakePanel, n int) {
		if n > 1 {
			p.failWritesUntilReopen = false
		}
	}

	s := &volumeSession{app: a}
	if err := s.open(); err != nil {
		t.Fatal(err)
	}

	if err := s.write(0x62, 40); err != nil {
		t.Fatalf("write should have recovered on reopen: %v", err)
	}
	if opener.opens != 2 {
		t.Errorf("opens = %d, want 2 (the original plus one reopen)", opener.opens)
	}
	if w := opener.panel.lastWrite(t); !w.Once {
		t.Error("the volume path must use SetOnce, not the retrying Set")
	}
}

func TestVolumeSessionGivesUpAfterOneReopen(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, liveOpts())
	opener.panel.failWritesUntilReopen = true

	s := &volumeSession{app: a}
	if err := s.open(); err != nil {
		t.Fatal(err)
	}
	if err := s.write(0x62, 40); err == nil {
		t.Fatal("a permanently failing panel should surface the error")
	}
	if opener.opens != 2 {
		t.Errorf("opens = %d, want 2: it must not retry forever", opener.opens)
	}
}

func TestVolumeSessionReadNeedsAnOpenPanel(t *testing.T) {
	a, _, _, _ := newFakeApp(t, liveOpts())

	s := &volumeSession{app: a}
	if s.ready() {
		t.Error("an unopened session reported ready")
	}
	if _, err := s.read(0x62); !errors.Is(err, ddc.ErrNoMonitors) {
		t.Errorf("read on an unopened session = %v, want ErrNoMonitors", err)
	}
}

func TestVolumeSessionCloseIsIdempotent(t *testing.T) {
	a, opener, _, _ := newFakeApp(t, liveOpts())

	s := &volumeSession{app: a}
	if err := s.open(); err != nil {
		t.Fatal(err)
	}
	s.close()
	s.close()

	if opener.panel.closes != 1 {
		t.Errorf("closes = %d, want 1", opener.panel.closes)
	}
}
