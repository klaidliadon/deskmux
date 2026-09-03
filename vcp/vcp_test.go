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
		{"wide level keeps both bytes", Level(0x0102).String(), "258 (0x102)"},
		{"zero level", Level(0).String(), "0 (0x00)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// These types implement fmt.Stringer, and fmt routes the hex verbs through
// Stringer: it hex-encodes the text of the string rather than the numeric
// value. Formatting Code(0x60) with a padded hex verb therefore yields
// "30783630" -- the ASCII of "0x60" -- which once reached real output as
//
//	0x30783630: current=15 (0x313520283078304629) max=18
//
// Neither the compiler nor go vet objects, because the construct is legal.
// Use the string verb, or convert to the underlying integer first.
func TestHexVerbsEncodeTheStringNotTheNumber(t *testing.T) {
	if got := fmt.Sprintf("%02X", Code(0x60)); got != "30783630" {
		t.Errorf("hex verb on Code produced %q; if fmt no longer routes hex "+
			"verbs through Stringer, this warning is obsolete", got)
	}

	// Converting first is the escape hatch where a bare number is wanted.
	if got := fmt.Sprintf("%02X", byte(Code(0x60))); got != "60" {
		t.Errorf("hex verb on the underlying byte = %q, want %q", got, "60")
	}
}
