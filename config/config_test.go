package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klaidliadon/lginput/vcp"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("built-in defaults do not validate: %v", err)
	}
}

// The shipped template must parse and validate, or `config init` hands users
// a file that immediately fails to load.
func TestTemplateParsesAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := WriteTemplate(path); err != nil {
		t.Fatalf("WriteTemplate: %v", err)
	}

	cfg, used, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if used != path {
		t.Errorf("used = %q, want %q", used, path)
	}

	// Spot-check values that the template documents.
	if cfg.Inputs.VCP != 0xF4 {
		t.Errorf("inputs.vcp = %s, want 0xF4", cfg.Inputs.VCP)
	}
	if cfg.Inputs.SourceAddr != 0x50 {
		t.Errorf("inputs.source_addr = %s, want 0x50", cfg.Inputs.SourceAddr)
	}
	if got := cfg.Inputs.Targets["usb-c"]; got != 0xD1 {
		t.Errorf("targets[usb-c] = %s, want 0xD1", got)
	}
	if cfg.VolumeKeys.Coalesce.D() != 20*time.Millisecond {
		t.Errorf("coalesce = %s, want 20ms", cfg.VolumeKeys.Coalesce.D())
	}
}

func TestWriteTemplateRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := WriteTemplate(path); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteTemplate(path); err == nil {
		t.Fatal("second write overwrote an existing file")
	}
}

// A partial file must override only what it mentions, leaving the rest at
// defaults. Users are expected to write short config files.
func TestLoadOverlaysOntoDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "monitor: 2\nvolume_keys:\n  step: 7\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Monitor != 2 {
		t.Errorf("monitor = %d, want 2", cfg.Monitor)
	}
	if cfg.VolumeKeys.Step != 7 {
		t.Errorf("step = %d, want 7", cfg.VolumeKeys.Step)
	}
	if cfg.VolumeKeys.AudioMatch != Default().VolumeKeys.AudioMatch {
		t.Errorf("audio_match = %q, should have kept the default", cfg.VolumeKeys.AudioMatch)
	}
	if cfg.Inputs.VCP != Default().Inputs.VCP {
		t.Errorf("inputs.vcp = %s, should have kept the default", cfg.Inputs.VCP)
	}
}

func TestLoadMissingExplicitPathIsAnError(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("monitor: [not an int]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("malformed YAML was accepted")
	}
}

func TestDurationRequiresAString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("ddc:\n  settle: 250\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("a bare number was accepted as a duration")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default", func(*Config) {}, false},
		{"negative monitor", func(c *Config) { c.Monitor = -1 }, true},
		{"no inputs", func(c *Config) { c.Inputs.Targets = nil }, true},
		{"unset input vcp", func(c *Config) { c.Inputs.VCP = 0 }, true},
		{"alias to nowhere", func(c *Config) { c.Inputs.Aliases["x"] = "nope" }, true},
		{"zero step", func(c *Config) { c.VolumeKeys.Step = 0 }, true},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, true},
		{"bad log level", func(c *Config) { c.Log.Level = "chatty" }, true},
		{"empty log fields are allowed", func(c *Config) { c.Log = Log{} }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected an error, got none")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestResolveInput(t *testing.T) {
	cfg := Default()

	tests := []struct {
		in      string
		want    vcp.Level
		wantErr bool
	}{
		{"usb-c", 0xD1, false},
		{"usbc", 0xD1, false}, // alias
		{"dp2", 0xD1, false},  // alias onto the same value
		{"dp", 0xD0, false},
		{"hdmi", 0x90, false}, // alias
		{"hdmi2", 0x91, false},
		{"vga", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := cfg.ResolveInput(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveInput(%q) = %s, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveInput(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ResolveInput(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
