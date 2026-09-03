package nvapi

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/klaidliadon/deskmux/vcp"
)

// The expected bytes are the packets verified against a real LG UltraGear
// 45GX950A: sending them switched the panel. If a refactor changes the
// checksum or byte order, these catch it before the hardware does.
func TestBuildSetVCP(t *testing.T) {
	tests := []struct {
		name   string
		source vcp.SourceAddr
		code   vcp.Code
		value  vcp.Level
		want   string
	}{
		{"lg input hdmi1", 0x50, 0xF4, 0x90, "50 84 03 F4 00 90 DD"},
		{"lg input hdmi2", 0x50, 0xF4, 0x91, "50 84 03 F4 00 91 DC"},
		{"lg input displayport", 0x50, 0xF4, 0xD0, "50 84 03 F4 00 D0 9D"},
		{"lg input usb-c", 0x50, 0xF4, 0xD1, "50 84 03 F4 00 D1 9C"},
		{"pbp off at standard address", vcp.Standard, 0xD7, 0x01, "51 84 03 D7 00 01 6E"},
		{"two-byte value keeps high byte", vcp.Standard, 0x62, 0x0102, "51 84 03 62 01 02 D9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildSetVCP(tt.source, tt.code, tt.value)

			want, err := hex.DecodeString(strings.ReplaceAll(tt.want, " ", ""))
			if err != nil {
				t.Fatalf("bad expectation %q: %v", tt.want, err)
			}
			if string(got) != string(want) {
				t.Errorf("BuildSetVCP(%s, %s, %s)\n got %X\nwant %X",
					tt.source, tt.code, tt.value, got, want)
			}
		})
	}
}

// The checksum covers the destination address even though it never appears in
// the buffer, which is the detail most likely to be lost in a rewrite.
func TestBuildSetVCPChecksumIncludesDeviceAddr(t *testing.T) {
	pkt := BuildSetVCP(vcp.Standard, 0x62, 30)

	checksum := vcp.DeviceAddr
	for _, b := range pkt[:len(pkt)-1] {
		checksum ^= b
	}
	if got := pkt[len(pkt)-1]; got != checksum {
		t.Errorf("checksum = 0x%02X, want 0x%02X", got, checksum)
	}

	// XOR of the whole packet plus the device address must cancel to zero.
	var acc = vcp.DeviceAddr
	for _, b := range pkt {
		acc ^= b
	}
	if acc != 0 {
		t.Errorf("packet does not self-cancel: 0x%02X", acc)
	}
}

func TestBuildSetVCPLength(t *testing.T) {
	pkt := BuildSetVCP(vcp.Standard, 0x10, 50)

	if len(pkt) != 7 {
		t.Fatalf("len = %d, want 7", len(pkt))
	}
	if pkt[1] != 0x84 {
		t.Errorf("length byte = 0x%02X, want 0x84 (0x80 | 4 data bytes)", pkt[1])
	}
	if pkt[2] != 0x03 {
		t.Errorf("opcode = 0x%02X, want 0x03 (set VCP feature)", pkt[2])
	}
}
