// Package vcp defines the primitive types of the DDC/CI protocol.
//
// These exist to stop the three quantities involved from being
// interchangeable. A feature code, the value written to it and the DDC source
// address the packet is sent with are all small unsigned integers, so with
// plain byte and uint16 the compiler happily accepts them in the wrong order
// and the mistake surfaces only as a monitor doing something unexpected. The
// named types make that a compile error.
package vcp

import "fmt"

// Code is a VCP feature code, such as 0x62 for audio volume.
type Code byte

func (c Code) String() string { return fmt.Sprintf("0x%02X", byte(c)) }

// Level is the value written to or read from a feature code. DDC/CI carries
// it as two bytes, high then low.
type Level uint16

func (l Level) String() string { return fmt.Sprintf("%d (0x%02X)", uint16(l), uint16(l)) }

// SourceAddr is the DDC source address a packet is sent with.
//
// Standard is what every conventional tool uses and what Windows hardcodes.
// Some monitors expose a manufacturer sidechannel at a different address --
// notably recent LG panels, which move input selection to 0x50 and silently
// ignore writes at 0x51.
type SourceAddr byte

func (s SourceAddr) String() string { return fmt.Sprintf("0x%02X", byte(s)) }

const (
	// Standard is the ordinary DDC/CI source address.
	Standard SourceAddr = 0x51

	// DeviceAddr is the DDC/CI destination (I2C 0x37 << 1). It is not part
	// of a packet body but is folded into the checksum.
	DeviceAddr byte = 0x6E
)

// Well-known MCCS feature codes. Monitors may or may not honour any of them,
// and the configuration can override which code a given operation uses.
const (
	Brightness Code = 0x10
	Contrast   Code = 0x12
	InputStd   Code = 0x60
	Volume     Code = 0x62
	Mute       Code = 0x8D
	PowerMode  Code = 0xD6
)
