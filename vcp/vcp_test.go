package vcp

import (
	"fmt"
	"testing"
)

func TestStringFormatting(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"code", Code(0x62).String(), "0x62"},
		{"code pads to two digits", Code(0x06).String(), "0x06"},
		{"source address", Standard.String(), "0x51"},
		{"lg sidechannel", SourceAddr(0x50).String(), "0x50"},
		{"level shows decimal and hex", Level(30).String(), "30 (0x1E)"},
		{"wide level", Level(0x0102).String(), "258 (0x102)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// These types implement fmt.Stringer, and fmt routes %x and %X through
// Stringer -- it hex-encodes the *text* of the string rather than the numeric
// value. So fmt.Sprintf("%02X", Code(0x60)) yields "30783630", the ASCII of
// "0x60", which once reached real output as:
//
//	0x30783630: current=15 (0x313520283078304629) max=18
//
// Use %s (or convert to the underlying integer) and never %x/%X. go vet does
// not flag this: it is legal, just never what anyone means.
func TestHexVerbsAreNotUsableOnTheseTypes(t *testing.T) {
	if got := fmt.Sprintf("%02X", Code(0x60)); got == "60" {
		t.Skip("fmt no longer routes %X through Stringer; the warning above is obsolete")
	} else if got != "30783630" {
		t.Errorf("unexpected %%X behaviour for Code: %q", got)
	}

	// The supported spelling.
	if got := fmt.Sprintf("%s", Code(0x60)); got != "0x60" {
		t.Errorf("%%s on Code = %q, want %q", got, "0x60")
	}
}
