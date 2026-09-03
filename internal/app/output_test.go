package app

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/klaidliadon/deskmux/config"
)

// The formatting bug that motivated this file printed the ASCII of a register
// name where the register was meant to go, and nothing caught it: the
// compiler is happy, go vet is happy, and no test looked at what the commands
// actually write. These do.
//
// Only paths that stop before touching hardware are covered -- dry runs and
// argument validation. Anything that opens a monitor needs a monitor.
func newTestApp(t *testing.T, opts Options) (*App, *bytes.Buffer) {
	t.Helper()

	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	return New(config.Default(), logger, &out, opts), &out
}

func TestInputDryRunOutput(t *testing.T) {
	// The packet bytes are the ones verified against a real LG 45GX950A, so
	// this pins the whole path from input name through config lookup to
	// assembled packet.
	tests := []struct {
		input string
		want  string
	}{
		{"usb-c", "packet: 50 84 03 F4 00 D1 9C   (source=0x50 code=0xF4 value=209 (0xD1))"},
		{"dp", "packet: 50 84 03 F4 00 D0 9D   (source=0x50 code=0xF4 value=208 (0xD0))"},
		{"hdmi1", "packet: 50 84 03 F4 00 90 DD   (source=0x50 code=0xF4 value=144 (0x90))"},
		{"hdmi2", "packet: 50 84 03 F4 00 91 DC   (source=0x50 code=0xF4 value=145 (0x91))"},
		{"usbc", "packet: 50 84 03 F4 00 D1 9C   (source=0x50 code=0xF4 value=209 (0xD1))"},
		{"USB-C", "packet: 50 84 03 F4 00 D1 9C   (source=0x50 code=0xF4 value=209 (0xD1))"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			app, out := newTestApp(t, Options{DryRun: true, Monitor: -1})

			if err := app.Input([]string{tt.input}); err != nil {
				t.Fatalf("Input(%q): %v", tt.input, err)
			}

			got := out.String()
			if !strings.Contains(got, tt.want) {
				t.Errorf("output missing expected line.\n got: %s\nwant: %s", got, tt.want)
			}
			if !strings.Contains(got, "dry-run: nothing sent") {
				t.Errorf("dry run did not say it sent nothing:\n%s", got)
			}
		})
	}
}

func TestRawDryRunHonoursSourceOverride(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			"defaults to the standard address",
			[]string{"0xD7", "1"},
			"packet: 51 84 03 D7 00 01 6E   (source=0x51 code=0xD7 value=1 (0x01))",
		},
		{
			"explicit sidechannel address",
			[]string{"0xF4", "0xD1", "0x50"},
			"packet: 50 84 03 F4 00 D1 9C   (source=0x50 code=0xF4 value=209 (0xD1))",
		},
		{
			"decimal arguments",
			[]string{"98", "30"},
			"packet: 51 84 03 62 00 1E C4   (source=0x51 code=0x62 value=30 (0x1E))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, out := newTestApp(t, Options{DryRun: true, Monitor: -1})

			if err := app.Raw(tt.args); err != nil {
				t.Fatalf("Raw(%v): %v", tt.args, err)
			}
			if got := out.String(); !strings.Contains(got, tt.want) {
				t.Errorf("output missing expected line.\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// This is the case the formatting bug actually corrupted.
func TestTableDryRunOutput(t *testing.T) {
	cfg := config.Default()

	tests := []struct {
		name  string
		table config.Table
		label string
		arg   string
		want  string
	}{
		{"pbp off", cfg.PBP, "pbp", "off", "pbp off: 0xD7 = 1 (0x01)"},
		{"pbp 50/50", cfg.PBP, "pbp", "50", "pbp 50: 0xD7 = 5 (0x05)"},
		{"power on", cfg.Power, "power", "on", "power on: 0xD6 = 1 (0x01)"},
		{"power off", cfg.Power, "power", "off", "power off: 0xD6 = 4 (0x04)"},
		{"mode is case-insensitive", cfg.Power, "power", "OFF", "power OFF: 0xD6 = 4 (0x04)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, out := newTestApp(t, Options{DryRun: true, Monitor: -1})

			if err := app.Table([]string{tt.arg}, tt.table, tt.label); err != nil {
				t.Fatalf("Table: %v", err)
			}
			if got := strings.TrimSpace(out.String()); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestArgumentValidation(t *testing.T) {
	cfg := config.Default()

	tests := []struct {
		name string
		call func(a *App) error
		want string // substring the error must mention
	}{
		{"input with no argument", func(a *App) error { return a.Input(nil) }, "usage"},
		{"input with two arguments", func(a *App) error { return a.Input([]string{"dp", "usb-c"}) }, "usage"},
		{"unknown input", func(a *App) error { return a.Input([]string{"vga"}) }, "unknown input"},
		{"raw with one argument", func(a *App) error { return a.Raw([]string{"0x60"}) }, "usage"},
		{"raw with four arguments", func(a *App) error { return a.Raw([]string{"1", "2", "3", "4"}) }, "usage"},
		{"raw with a bad code", func(a *App) error { return a.Raw([]string{"zz", "1"}) }, "bad VCP code"},
		{"raw with a bad value", func(a *App) error { return a.Raw([]string{"0x60", "zz"}) }, "bad value"},
		{"raw with a bad source", func(a *App) error { return a.Raw([]string{"0x60", "1", "zz"}) }, "bad source address"},
		{"code out of byte range", func(a *App) error { return a.Raw([]string{"0x1FF", "1"}) }, "bad VCP code"},
		{"value out of range", func(a *App) error { return a.Raw([]string{"0x60", "0x1FFFF"}) }, "bad value"},
		{"unknown pbp mode", func(a *App) error { return a.Table([]string{"sideways"}, cfg.PBP, "pbp") }, "unknown pbp mode"},
		{"table with no argument", func(a *App) error { return a.Table(nil, cfg.PBP, "pbp") }, "usage"},
		{"config with no subcommand", func(a *App) error { return a.Config(nil) }, "usage"},
		{"unknown config subcommand", func(a *App) error { return a.Config([]string{"reset"}) }, "unknown config subcommand"},
		{"devices with two arguments", func(a *App) error { return a.Devices([]string{"a", "b"}) }, "usage"},
		{"service with no subcommand", func(a *App) error { return a.Service(nil) }, "usage"},
		{"unknown service subcommand", func(a *App) error { return a.Service([]string{"restart"}) }, "unknown service subcommand"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := newTestApp(t, Options{DryRun: true, Monitor: -1})

			err := tt.call(app)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// An unknown input must list what is available, or the message is useless to
// someone whose monitor uses different names.
func TestUnknownInputListsTheConfiguredOnes(t *testing.T) {
	app, _ := newTestApp(t, Options{DryRun: true, Monitor: -1})

	err := app.Input([]string{"vga"})
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, name := range []string{"hdmi1", "hdmi2", "dp", "usb-c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention configured input %q: %v", name, err)
		}
	}
}

func TestServiceInstallDryRunOutput(t *testing.T) {
	app, out := newTestApp(t, Options{DryRun: true, Monitor: -1})

	if err := app.Service([]string{"install"}); err != nil {
		t.Fatalf("service install: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		`reg add HKCU\Software\Microsoft\Windows\CurrentVersion\Run`,
		`/v "deskmux watch"`,
		`/v "deskmux volumekeys"`,
		"/t REG_SZ",
		"-log", // each entry gets its own log, having no console
	} {
		if !strings.Contains(got, want) {
			t.Errorf("service install output missing %q:\n%s", want, got)
		}
	}
}

func TestConfigShowEmitsTheTemplate(t *testing.T) {
	app, out := newTestApp(t, Options{Monitor: -1})

	if err := app.Config([]string{"show"}); err != nil {
		t.Fatalf("config show: %v", err)
	}

	got := out.String()
	for _, want := range []string{"inputs:", "source_addr: 0x50", "volume_keys:", "watch:"} {
		if !strings.Contains(got, want) {
			t.Errorf("template missing %q", want)
		}
	}
}

func TestMonitorIndexOverride(t *testing.T) {
	cfg := config.Default()
	cfg.Monitor = 3
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if got := New(cfg, logger, io.Discard, Options{Monitor: -1}).monitorIndex(); got != 3 {
		t.Errorf("with no override, index = %d, want the configured 3", got)
	}
	if got := New(cfg, logger, io.Discard, Options{Monitor: 1}).monitorIndex(); got != 1 {
		t.Errorf("with an override, index = %d, want 1", got)
	}
	if got := New(cfg, logger, io.Discard, Options{Monitor: 0}).monitorIndex(); got != 0 {
		t.Errorf("zero is a valid override, index = %d, want 0", got)
	}
}
