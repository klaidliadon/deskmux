package app

import (
	"errors"
	"testing"
	"time"

	"github.com/klaidliadon/deskmux/ddc"
	"github.com/klaidliadon/deskmux/nvapi"
	"github.com/klaidliadon/deskmux/vcp"
)

// The fakes below stand in for the two hardware seams so the command layer can
// be exercised without a monitor. They are written to be able to misbehave the
// way the real hardware misbehaves -- a register that accepts writes and
// ignores them, a DDC engine that has wedged, a bus that accepts nothing --
// because those are the paths that have historically been wrong.

// fakeWrite records one write for later assertion.
type fakeWrite struct {
	Code  vcp.Code
	Value vcp.Level
	Once  bool // SetOnce rather than Set
}

type fakePanel struct {
	name string

	// readings is the register file. A code absent from it is unsupported and
	// reads fail, as an unsupported code does on real hardware.
	readings map[vcp.Code]ddc.Reading

	// ignored codes accept writes and never change value: the exact behaviour
	// of VCP 0x60 on the panel this tool exists for.
	ignored map[vcp.Code]bool

	// setErr and getErr force failures for specific codes.
	setErr map[vcp.Code]error
	getErr map[vcp.Code]error

	caps    string
	capsErr error

	writes []fakeWrite
	closes int

	// failWritesUntilReopen models a stale handle: every write fails until the
	// panel is closed and a fresh one opened.
	failWritesUntilReopen bool
}

func newFakePanel() *fakePanel {
	return &fakePanel{
		name: "FAKE PANEL",
		readings: map[vcp.Code]ddc.Reading{
			0x10: {Current: 100, Max: 100}, // brightness
			0x12: {Current: 50, Max: 100},  // contrast
			0x62: {Current: 27, Max: 100},  // volume
			0x8D: {Current: 2, Max: 2},     // mute
			0xD6: {Current: 1, Max: 4},     // power
			0xD7: {Current: 1, Max: 5},     // pbp
		},
		ignored: map[vcp.Code]bool{},
		setErr:  map[vcp.Code]error{},
		getErr:  map[vcp.Code]error{},
		caps:    "(prot(monitor)type(LCD)model(FAKE)vcp(10 12 60(11 12 0F 10 ) 62 ))",
	}
}

func (p *fakePanel) Name() string { return p.name }

func (p *fakePanel) Get(code vcp.Code) (ddc.Reading, error) {
	if err := p.getErr[code]; err != nil {
		return ddc.Reading{}, err
	}
	r, ok := p.readings[code]
	if !ok {
		return ddc.Reading{}, errors.New("GetVCPFeatureAndVCPFeatureReply: unsupported")
	}
	return r, nil
}

func (p *fakePanel) set(code vcp.Code, value vcp.Level, once bool) error {
	if p.failWritesUntilReopen {
		return errors.New("error receiving data from the device on the I2C bus")
	}
	if err := p.setErr[code]; err != nil {
		return err
	}

	p.writes = append(p.writes, fakeWrite{Code: code, Value: value, Once: once})

	// The write is accepted either way; only the effect differs.
	if !p.ignored[code] {
		r := p.readings[code]
		r.Current = value
		if r.Max == 0 {
			r.Max = 100
		}
		p.readings[code] = r
	}
	return nil
}

func (p *fakePanel) Set(code vcp.Code, value vcp.Level) error {
	return p.set(code, value, false)
}

func (p *fakePanel) SetOnce(code vcp.Code, value vcp.Level) error {
	return p.set(code, value, true)
}

func (p *fakePanel) SetVerified(code vcp.Code, value vcp.Level, _ time.Duration) (bool, vcp.Level, error) {
	if err := p.Set(code, value); err != nil {
		return false, 0, err
	}
	r, err := p.Get(code)
	if err != nil {
		return false, 0, err
	}
	return r.Current == value, r.Current, nil
}

func (p *fakePanel) Capabilities() (string, error) {
	if p.capsErr != nil {
		return "", p.capsErr
	}
	return p.caps, nil
}

func (p *fakePanel) Close() { p.closes++ }

// lastWrite is the most recent write, for the common single-write assertion.
func (p *fakePanel) lastWrite(t *testing.T) fakeWrite {
	t.Helper()
	if len(p.writes) == 0 {
		t.Fatal("no writes were made")
	}
	return p.writes[len(p.writes)-1]
}

// fakeOpener hands out panels and counts opens, so tests can assert that
// handles are released rather than leaked.
type fakeOpener struct {
	panel *fakePanel
	err   error

	opens   int
	indexes []int

	// onOpen, if set, runs before each open and may mutate the panel. It is
	// how a stale handle is made to recover on reopen.
	onOpen func(p *fakePanel, n int)
}

func (o *fakeOpener) Open(index int) (Panel, error) {
	o.opens++
	o.indexes = append(o.indexes, index)

	if o.err != nil {
		return nil, o.err
	}
	if o.onOpen != nil {
		o.onOpen(o.panel, o.opens)
	}
	return o.panel, nil
}

// fakeBus records raw I2C packets. accepted controls how many of the reported
// attempts succeeded, since "the bus took it" is the only signal the real
// sidechannel gives.
type fakeBus struct {
	err      error
	accepted int
	total    int

	packets []([]byte)
	opts    []nvapi.WriteOptions
}

func newFakeBus() *fakeBus { return &fakeBus{accepted: 1, total: 1} }

func (b *fakeBus) Write(pkt []byte, opts nvapi.WriteOptions) ([]nvapi.Attempt, error) {
	b.packets = append(b.packets, append([]byte(nil), pkt...))
	b.opts = append(b.opts, opts)

	if b.err != nil {
		return nil, b.err
	}

	attempts := make([]nvapi.Attempt, 0, b.total)
	for i := range b.total {
		attempts = append(attempts, nvapi.Attempt{
			GPU:    0,
			Mask:   0x4000,
			OK:     i < b.accepted,
			Status: "NVAPI_OK (0)",
		})
	}
	return attempts, nil
}

func (b *fakeBus) lastPacket(t *testing.T) []byte {
	t.Helper()
	if len(b.packets) == 0 {
		t.Fatal("no packets were sent")
	}
	return b.packets[len(b.packets)-1]
}
