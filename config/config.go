// Package config defines deskmux's on-disk configuration.
//
// Everything hardware-specific lives here rather than in code: the VCP
// registers, the DDC source addresses and the per-input values all vary
// across monitors, so a user with a different LG model should be able to
// adapt the tool by editing YAML rather than recompiling.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/klaidliadon/deskmux/vcp"
)

// Duration wraps time.Duration so durations can be written as "250ms" in
// YAML, which yaml.v3 does not support natively.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalYAML writes the duration back as a string such as "250ms".
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// UnmarshalYAML accepts a duration string. A bare number is rejected: it
// would silently be read as nanoseconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"250ms\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the whole configuration tree.
type Config struct {
	Monitor    int        `yaml:"monitor"`
	DDC        DDC        `yaml:"ddc"`
	Registers  Registers  `yaml:"registers"`
	Inputs     Inputs     `yaml:"inputs"`
	PBP        Table      `yaml:"pbp"`
	Power      Table      `yaml:"power"`
	Watch      Watch      `yaml:"watch"`
	VolumeKeys VolumeKeys `yaml:"volume_keys"`
	Log        Log        `yaml:"log"`
}

// DDC holds timings for the standard DDC/CI path.
type DDC struct {
	// Settle is how long to wait before a verification read-back.
	Settle Duration `yaml:"settle"`
	// BusDelay is the pause between raw I2C writes. Going much below 40ms
	// risks wedging panels with fragile DDC engines.
	BusDelay Duration `yaml:"bus_delay"`

	// SourceAddr is the DDC source address for ordinary writes. Only the
	// input section normally departs from the standard 0x51.
	SourceAddr vcp.SourceAddr `yaml:"source_addr"`
}

// Registers names the standard VCP codes this tool reads and writes.
type Registers struct {
	Brightness    vcp.Code `yaml:"brightness"`
	Contrast      vcp.Code `yaml:"contrast"`
	Volume        vcp.Code `yaml:"volume"`
	Mute          vcp.Code `yaml:"mute"`
	InputStandard vcp.Code `yaml:"input_standard"`
}

// Inputs describes how this monitor selects its input source.
//
// On most monitors that is VCP 0x60 at the standard source address 0x51. On
// recent LG panels 0x60 is advertised and then silently ignored, and the real
// control is VCP 0xF4 at source address 0x50.
type Inputs struct {
	VCP        vcp.Code             `yaml:"vcp"`
	SourceAddr vcp.SourceAddr       `yaml:"source_addr"`
	Targets    map[string]vcp.Level `yaml:"targets"`
	Aliases    map[string]string    `yaml:"aliases"`
}

// Table is a VCP register plus its named values.
type Table struct {
	VCP        vcp.Code             `yaml:"vcp"`
	SourceAddr vcp.SourceAddr       `yaml:"source_addr"`
	Modes      map[string]vcp.Level `yaml:"modes"`
}

// Profile is what to apply on a dock transition.
type Profile struct {
	// Input to select, or empty to leave the input alone.
	Input string `yaml:"input"`
	// Volume to set, or -1 to leave it alone.
	Volume int `yaml:"volume"`
	// PowerOff powers the monitor down after applying the rest.
	PowerOff bool `yaml:"power_off"`
}

// Watch configures the dock watcher.
type Watch struct {
	// Match lists device-ID substrings that identify the dock. Find them
	// with `deskmux devices <substring>`.
	Match []string `yaml:"match"`
	Poll  Duration `yaml:"poll"`

	// Debounce is how many consecutive polls a reading must hold before it
	// counts as a change. Device arrival enumerates as a burst as each
	// function of a dock appears, so acting on the first sighting races the
	// rest. Raise it if a flaky dock triggers spurious switches.
	Debounce int     `yaml:"debounce"`
	OnDock   Profile `yaml:"on_dock"`
	OnUndock Profile `yaml:"on_undock"`
}

// VolumeKeys configures volume-key interception.
type VolumeKeys struct {
	Step       int      `yaml:"step"`
	PinWindows bool     `yaml:"pin_windows"`
	Coalesce   Duration `yaml:"coalesce"`
	AudioMatch string   `yaml:"audio_match"`
	AudioPoll  Duration `yaml:"audio_poll"`
	Resync     Duration `yaml:"resync"`
}

// Log configures slog.
type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text or json
	File   string `yaml:"file"`   // also append structured logs here
}

// Default returns the built-in configuration, tuned for the LG UltraGear
// 45GX950A, which is the only panel this has been verified against.
func Default() Config {
	return Config{
		Monitor: 0,
		DDC: DDC{
			Settle:     Duration(250 * time.Millisecond),
			BusDelay:   Duration(40 * time.Millisecond),
			SourceAddr: vcp.Standard,
		},
		Registers: Registers{
			Brightness:    0x10,
			Contrast:      0x12,
			Volume:        0x62,
			Mute:          0x8D,
			InputStandard: 0x60,
		},
		Inputs: Inputs{
			VCP:        0xF4,
			SourceAddr: 0x50,
			Targets: map[string]vcp.Level{
				"hdmi1": 0x90,
				"hdmi2": 0x91,
				"dp":    0xD0,
				"usb-c": 0xD1,
			},
			Aliases: map[string]string{
				"usbc": "usb-c", "typec": "usb-c", "type-c": "usb-c", "tb": "usb-c",
				"dp1": "dp", "displayport": "dp", "dp2": "usb-c", "hdmi": "hdmi1",
			},
		},
		PBP: Table{
			VCP:        0xD7,
			SourceAddr: 0x51,
			Modes:      map[string]vcp.Level{"off": 0x01, "50": 0x05, "66": 0x03},
		},
		Power: Table{
			VCP:        0xD6,
			SourceAddr: 0x51,
			Modes:      map[string]vcp.Level{"on": 0x01, "off": 0x04},
		},
		Watch: Watch{
			Match:    []string{"VID_1E91", "VEN_OWC_TB3", "SUBSYS_00191C7A"},
			Poll:     Duration(2 * time.Second),
			Debounce: 2,
			OnDock:   Profile{Input: "usb-c", Volume: -1},
			OnUndock: Profile{Input: "dp", Volume: -1},
		},
		VolumeKeys: VolumeKeys{
			Step:       1,
			PinWindows: true,
			Coalesce:   Duration(20 * time.Millisecond),
			AudioMatch: "ULTRAGEAR",
			AudioPoll:  Duration(2 * time.Second),
			Resync:     Duration(10 * time.Second),
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// ErrNotFound reports that no configuration file exists. Callers normally
// treat this as "use defaults" rather than as a failure.
var ErrNotFound = errors.New("no configuration file found")

// SearchPaths lists where Load looks when given no explicit path.
func SearchPaths() []string {
	var paths []string
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "deskmux", "config.yaml"))
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "deskmux.yaml"))
	}
	paths = append(paths, "deskmux.yaml")
	return paths
}

// Load reads configuration, starting from Default so a partial file only
// overrides what it mentions. An empty path searches SearchPaths.
//
// The returned path says which file was used, empty if none was.
func Load(path string) (Config, string, error) {
	cfg := Default()

	candidates := []string{path}
	if path == "" {
		candidates = SearchPaths()
	}

	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cfg, "", fmt.Errorf("read %s: %w", candidate, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, candidate, fmt.Errorf("parse %s: %w", candidate, err)
		}
		if err := cfg.Validate(); err != nil {
			return cfg, candidate, fmt.Errorf("%s: %w", candidate, err)
		}
		return cfg, candidate, nil
	}

	if path != "" {
		return cfg, "", fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return cfg, "", nil
}

// Validate catches the mistakes that would otherwise surface as confusing
// hardware behaviour rather than as an error.
func (c Config) Validate() error {
	if c.Monitor < 0 {
		return fmt.Errorf("monitor index %d is negative", c.Monitor)
	}
	if len(c.Inputs.Targets) == 0 {
		return errors.New("inputs.targets is empty: no input names are defined")
	}
	if c.Inputs.VCP == 0 {
		return errors.New("inputs.vcp is unset")
	}
	for alias, target := range c.Inputs.Aliases {
		if _, ok := c.Inputs.Targets[target]; !ok {
			return fmt.Errorf("inputs.aliases[%q] points at %q, which is not in inputs.targets", alias, target)
		}
	}
	if c.Watch.Debounce < 1 {
		return fmt.Errorf("watch.debounce must be at least 1, got %d", c.Watch.Debounce)
	}
	if c.VolumeKeys.Step <= 0 {
		return fmt.Errorf("volume_keys.step must be positive, got %d", c.VolumeKeys.Step)
	}
	if c.DDC.BusDelay.D() < 0 {
		return errors.New("ddc.bus_delay must not be negative")
	}
	switch c.Log.Format {
	case "", "text", "json":
	default:
		return fmt.Errorf("log.format %q: want text or json", c.Log.Format)
	}
	switch c.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level)
	}
	return nil
}

// ResolveInput maps a user-supplied input name, following aliases, to its
// VCP value.
func (c Config) ResolveInput(name string) (vcp.Level, error) {
	if alias, ok := c.Inputs.Aliases[name]; ok {
		name = alias
	}
	if v, ok := c.Inputs.Targets[name]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("unknown input %q (defined: %s)", name, joinKeys(c.Inputs.Targets))
}

func joinKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "none"
	}
	out := keys[0]
	for _, k := range keys[1:] {
		out += ", " + k
	}
	return out
}

// WriteTemplate writes the documented starter configuration to path,
// refusing to clobber an existing file.
func WriteTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(Template), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
