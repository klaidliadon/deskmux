package app

import (
	"strings"
	"syscall"
	"testing"

	"github.com/klaidliadon/lginput/ddc"
	"github.com/klaidliadon/lginput/vcp"
)

func TestResolveLevel(t *testing.T) {
	reading := ddc.Reading{Current: 40, Max: 100}

	tests := []struct {
		arg     string
		want    vcp.Level
		wantErr bool
	}{
		{"70", 70, false},
		{"+10", 50, false},
		{"-10", 30, false},
		{"0", 0, false},
		{"100", 100, false},
		{"+999", 100, false}, // clamped to max
		{"-999", 0, false},   // clamped to zero
		{"200", 100, false},  // absolute values clamp too
		{"", 0, true},
		{"loud", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			got, err := resolveLevel(tt.arg, reading)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveLevel(%q) = %s, want an error", tt.arg, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLevel(%q): %v", tt.arg, err)
			}
			if got != tt.want {
				t.Errorf("resolveLevel(%q) = %s, want %s", tt.arg, got, tt.want)
			}
		})
	}
}

// Dock arrival enumerates as a burst, so a single reading must not fire a
// profile. This is the guard against switching inputs on a glitch.
func TestDebouncer(t *testing.T) {
	t.Run("needs consecutive readings", func(t *testing.T) {
		d := newDebouncer(false, 2)

		if d.observe(true) {
			t.Fatal("fired on the first reading")
		}
		if !d.observe(true) {
			t.Fatal("did not fire on the second consecutive reading")
		}
		if d.observe(true) {
			t.Fatal("fired again without a state change")
		}
	})

	t.Run("a flapping reading never fires", func(t *testing.T) {
		d := newDebouncer(false, 2)
		for range 10 {
			if d.observe(true) || d.observe(false) {
				t.Fatal("flapping readings fired a transition")
			}
		}
	})

	t.Run("returns to the original state", func(t *testing.T) {
		d := newDebouncer(true, 2)

		d.observe(false)
		if !d.observe(false) {
			t.Fatal("did not fire on a settled change")
		}
		d.observe(true)
		if !d.observe(true) {
			t.Fatal("did not fire changing back")
		}
	})
}

func TestSplitMultiSz(t *testing.T) {
	encode := func(items ...string) []uint16 {
		var out []uint16
		for _, s := range items {
			u, err := syscall.UTF16FromString(s)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, u...) // UTF16FromString already NUL-terminates
		}
		return append(out, 0) // list terminator
	}

	tests := []struct {
		name string
		in   []uint16
		want []string
	}{
		{"empty", []uint16{0}, nil},
		{"one", encode("USB\\VID_1E91"), []string{"USB\\VID_1E91"}},
		{"several", encode("A", "BB", "CCC"), []string{"A", "BB", "CCC"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitMultiSz(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDeviceMatcher(t *testing.T) {
	m := newDeviceMatcher([]string{"vid_1e91", "  VEN_OWC_TB3  ", "", "   "})

	if got := len(m.needles); got != 2 {
		t.Fatalf("needles = %d, want 2 (blank entries dropped)", got)
	}
	for _, needle := range m.needles {
		if needle != strings.ToUpper(needle) {
			t.Errorf("needle %q was not folded at construction", needle)
		}
	}

	if newDeviceMatcher(nil).empty() != true {
		t.Error("nil match list should report empty")
	}
	if newDeviceMatcher([]string{" "}).empty() != true {
		t.Error("whitespace-only match list should report empty")
	}
	if m.empty() {
		t.Error("populated matcher reported empty")
	}
}

func TestVolumeStateClampsAndCoalesces(t *testing.T) {
	s := newVolumeState(50, 100)

	s.adjust(10)
	s.adjust(10)
	target, wantLevel, _, _ := s.pending()
	if !wantLevel {
		t.Fatal("no pending write after adjusting")
	}
	if target != 70 {
		t.Errorf("target = %d, want 70 (both adjustments coalesced)", target)
	}

	if _, wantLevel, _, _ := s.pending(); wantLevel {
		t.Error("pending stayed dirty after being taken")
	}

	s.adjust(1000)
	if target, _, _, _ := s.pending(); target != 100 {
		t.Errorf("target = %d, want 100 (clamped to max)", target)
	}

	s.adjust(-1000)
	if target, _, _, _ := s.pending(); target != 0 {
		t.Errorf("target = %d, want 0 (clamped to zero)", target)
	}
}

// A queued keypress must win over a level observed on the monitor, or a
// resync landing mid-keystroke would undo what the user just pressed.
func TestVolumeStateSyncDefersToPendingWrite(t *testing.T) {
	s := newVolumeState(50, 100)

	if !s.syncTo(60) {
		t.Fatal("clean state refused an external level")
	}

	s.adjust(5) // marks dirty
	if s.syncTo(20) {
		t.Fatal("external level overwrote a pending keypress")
	}

	if target, _, _, _ := s.pending(); target != 65 {
		t.Errorf("target = %d, want 65", target)
	}
}

func TestVolumeStateToggleMute(t *testing.T) {
	s := newVolumeState(50, 100)

	s.toggleMute()
	if _, _, wantMute, muted := s.pending(); !wantMute || !muted {
		t.Fatalf("first toggle: wantMute=%v muted=%v, want true/true", wantMute, muted)
	}

	s.toggleMute()
	if _, _, wantMute, muted := s.pending(); !wantMute || muted {
		t.Fatalf("second toggle: wantMute=%v muted=%v, want true/false", wantMute, muted)
	}
}
