package app

import (
	"fmt"
	"time"

	"github.com/klaidliadon/deskmux/ddc"
	"github.com/klaidliadon/deskmux/nvapi"
	"github.com/klaidliadon/deskmux/vcp"
)

// Panel is one monitor's standard DDC/CI control surface.
//
// The interface exists for testability, not for polymorphism: there is exactly
// one real implementation. Every command in this package used to reach
// ddc.Open directly, which meant nothing above argument parsing could be
// tested without the hardware attached -- and the bugs that reached the user
// were all in that untestable middle (a dry run that was not dry, a format
// verb that hex-encoded a register name, a fallback path that had never run).
//
// Panel is deliberately shaped like ddc.Monitor rather than like the commands
// that consume it, so the real implementation stays a thin forwarding shim
// with nowhere for behaviour to hide.
type Panel interface {
	// Name is the description Windows reports, for display only.
	Name() string

	Get(code vcp.Code) (ddc.Reading, error)

	// Set retries transient bus errors; SetOnce does not, for latency-
	// sensitive callers such as the volume keys.
	Set(code vcp.Code, value vcp.Level) error
	SetOnce(code vcp.Code, value vcp.Level) error

	// SetVerified writes, waits out settle, and reads back. landed reports
	// whether the monitor took the value, which is not the same as err == nil:
	// DDC/CI writes are fire-and-forget.
	SetVerified(code vcp.Code, value vcp.Level, settle time.Duration) (landed bool, readback vcp.Level, err error)

	Capabilities() (string, error)

	// Close releases the native handles. Safe to call more than once.
	Close()
}

// Opener opens the monitor at an index.
type Opener interface {
	Open(index int) (Panel, error)
}

// Bus is the raw I2C sidechannel: hand it a packet, learn what the hardware
// accepted. It never reports whether the monitor obeyed, because that channel
// does not acknowledge.
type Bus interface {
	Write(pkt []byte, opts nvapi.WriteOptions) ([]nvapi.Attempt, error)
}

// ddcOpener is the real Opener, over dxva2.dll.
type ddcOpener struct{}

func (ddcOpener) Open(index int) (Panel, error) {
	set, err := ddc.Open()
	if err != nil {
		return nil, err
	}

	if index < 0 || index >= len(set.Monitors) {
		set.Close()
		return nil, fmt.Errorf("monitor index %d out of range (%d found)", index, len(set.Monitors))
	}
	return &ddcPanel{set: set, mon: set.Monitors[index]}, nil
}

// ddcPanel owns the Set it was opened from, so callers close one thing rather
// than remembering that the handle and the monitor have separate lifetimes.
type ddcPanel struct {
	set *ddc.Set
	mon ddc.Monitor
}

func (p *ddcPanel) Name() string { return p.mon.Name }

func (p *ddcPanel) Get(code vcp.Code) (ddc.Reading, error) { return p.mon.GetVCP(code) }

func (p *ddcPanel) Set(code vcp.Code, value vcp.Level) error { return p.mon.SetVCP(code, value) }

func (p *ddcPanel) SetOnce(code vcp.Code, value vcp.Level) error {
	return p.mon.SetVCPOnce(code, value)
}

func (p *ddcPanel) SetVerified(code vcp.Code, value vcp.Level, settle time.Duration) (bool, vcp.Level, error) {
	return p.mon.SetVCPVerified(code, value, settle)
}

func (p *ddcPanel) Capabilities() (string, error) { return p.mon.Capabilities() }

func (p *ddcPanel) Close() {
	if p.set != nil {
		p.set.Close()
		p.set = nil
	}
}

// nvapiBus is the real Bus, over NvAPI_I2CWrite.
//
// Loading NVAPI is folded in rather than exposed, so the seam is the single
// question a caller actually has: send this packet, what happened? nvapi.Load
// caches success and retries after failure, so calling it per write is cheap
// and recovers from a driver restart.
type nvapiBus struct{}

func (nvapiBus) Write(pkt []byte, opts nvapi.WriteOptions) ([]nvapi.Attempt, error) {
	client, err := nvapi.Load()
	if err != nil {
		return nil, err
	}
	return client.Write(pkt, opts), nil
}
