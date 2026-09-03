// Package app implements deskmux's commands.
//
// It is internal on purpose: the reusable parts are the ddc, nvapi and
// winaudio packages. What lives here is policy -- which register to poke for
// a given command, when to hand a monitor between machines, how to react to a
// dock appearing.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/klaidliadon/deskmux/config"
	"github.com/klaidliadon/deskmux/nvapi"
	"github.com/klaidliadon/deskmux/vcp"
)

// Options are per-invocation overrides that do not belong in the config file.
type Options struct {
	DryRun  bool
	Verbose bool
	Fast    bool

	// Monitor overrides config.Monitor when non-negative.
	Monitor int
}

// App carries the configuration, logger and output sink for one run.
type App struct {
	cfg  config.Config
	log  *slog.Logger
	out  io.Writer
	opts Options

	// panels and bus are the two hardware seams. New wires them to the real
	// implementations; tests in this package substitute fakes by assignment,
	// which keeps dependency injection out of main for a program that has
	// exactly one real backend.
	panels Opener
	bus    Bus

	// wake turns this machine's own display output back on. A field rather
	// than a direct call so tests can observe it without synthesising real
	// input events on the developer's desktop.
	wake func() (time.Duration, error)
}

// New builds an App. out receives command results, which are program output
// rather than logs; log receives operational events.
func New(cfg config.Config, logger *slog.Logger, out io.Writer, opts Options) *App {
	return &App{
		cfg:    cfg,
		log:    logger,
		out:    out,
		opts:   opts,
		panels: ddcOpener{},
		bus:    nvapiBus{},
		wake:   wakeDisplay,
	}
}

// printf writes program output. Diagnostics go through a.log instead.
func (a *App) printf(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format, args...) }
func (a *App) println(args ...any)               { _, _ = fmt.Fprintln(a.out, args...) }

// ErrUsage asks the caller to print usage.
var ErrUsage = errors.New("no command given")

// Run dispatches one command. ctx cancels the long-running daemons.
func (a *App) Run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "probe":
		return a.Probe()
	case "get":
		return a.Get(args)
	case "set":
		return a.Set(args)
	case "brightness":
		return a.Level(args, a.cfg.Registers.Brightness, "brightness")
	case "volume":
		return a.Level(args, a.cfg.Registers.Volume, "volume")
	case "mute":
		return a.Mute(_muteOn)
	case "unmute":
		return a.Mute(_muteOff)
	case "input":
		return a.Input(args)
	case "pbp":
		return a.Table(args, a.cfg.PBP, "pbp")
	case "power":
		return a.Table(args, a.cfg.Power, "power")
	case "raw":
		return a.Raw(args)
	case "wake":
		return a.Wake()
	case "watch":
		return a.Watch(ctx)
	case "volumekeys":
		return a.VolumeKeys(ctx)
	case "devices":
		return a.Devices(args)
	case "config":
		return a.Config(args)
	case "service":
		return a.Service(args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// monitorIndex resolves the configured index against any CLI override.
func (a *App) monitorIndex() int {
	if a.opts.Monitor >= 0 {
		return a.opts.Monitor
	}
	return a.cfg.Monitor
}

// openPanel returns the selected monitor. The caller must close it.
func (a *App) openPanel() (Panel, error) {
	return a.panels.Open(a.monitorIndex())
}

func parseVCP(s string) (vcp.Code, error) {
	n, err := strconv.ParseUint(s, 0, 8)
	if err != nil {
		return 0, fmt.Errorf("bad VCP code %q: %w", s, err)
	}
	return vcp.Code(n), nil
}

func parseValue(s string) (vcp.Level, error) {
	n, err := strconv.ParseUint(s, 0, 16)
	if err != nil {
		return 0, fmt.Errorf("bad value %q: %w", s, err)
	}
	return vcp.Level(n), nil
}

// sendRaw builds a DDC packet and pushes it over NVAPI raw I2C.
func (a *App) sendRaw(source vcp.SourceAddr, code vcp.Code, value vcp.Level, label string) error {
	pkt := nvapi.BuildSetVCP(source, code, value)

	// "% X" on a byte slice is exactly the wanted rendering: two upper-case
	// hex digits per byte, single-space separated.
	a.printf("%s\n  packet: % X   (source=%s code=%s value=%s)\n",
		label, pkt, source, code, value)

	if a.opts.DryRun {
		a.println("  dry-run: nothing sent")
		return nil
	}

	attempts, err := a.bus.Write(pkt, nvapi.WriteOptions{
		Fast:  a.opts.Fast,
		Delay: a.cfg.DDC.BusDelay.D(),
		OnGPUError: func(gpu int, err error) {
			a.log.Warn("gpu enumeration failed", "gpu", gpu, "err", err)
		},
	})
	if err != nil {
		return err
	}

	accepted := nvapi.Accepted(attempts)

	if a.opts.Verbose {
		for _, at := range attempts {
			port := "-"
			if at.HasPort {
				port = strconv.Itoa(at.Port)
			}
			a.printf("    gpu=%d mask=0x%08X port=%s ok=%v %s\n", at.GPU, at.Mask, port, at.OK, at.Status)
		}
	}

	// Also log it: when a daemon calls this its stdout goes nowhere, and how
	// many writes the bus took is the only evidence available for a channel
	// that never acknowledges.
	a.log.Debug("i2c write",
		"label", label, "source", source, "code", code, "value", value,
		"accepted", accepted, "attempts", len(attempts))

	a.printf("  %d/%d writes accepted by the bus\n", accepted, len(attempts))

	// Nothing even attempted is a different failure from everything rejected,
	// and the two used to share one message. No attempts means NVAPI reported
	// no connected outputs, so there was no bus to write to -- which on a
	// hybrid laptop is a state the machine enters by itself: when the discrete
	// GPU parks, the mux hands the panel to the integrated one and NVAPI stops
	// seeing it, with the display still lit and working the whole time.
	if len(attempts) == 0 {
		return errors.New("no NVIDIA GPU reports a connected output, so there was no bus to write to; " +
			"on a hybrid laptop the display moves to the integrated GPU when the discrete one powers down " +
			"(nvidia-smi --query-gpu=display_active --format=csv reports which)")
	}
	if accepted == 0 {
		return errors.New("every I2C write was rejected; is the monitor on the NVIDIA GPU?")
	}

	a.println("  this channel never acknowledges -- confirm by looking at the screen")
	return nil
}

// setVolume writes VCP volume, preferring the verifiable DDC path and falling
// back to raw I2C when the Windows DDC layer is unavailable, which happens
// routinely on panels whose DDC engine has wedged.
func (a *App) setVolume(level vcp.Level) string {
	if a.opts.DryRun {
		return fmt.Sprintf("dry-run: would set %d", level)
	}

	if p, err := a.openPanel(); err == nil {
		defer p.Close()

		landed, readback, err := p.SetVerified(a.cfg.Registers.Volume, level, a.cfg.DDC.Settle.D())
		if err == nil {
			if landed {
				return fmt.Sprintf("%d via DDC, verified", readback)
			}
			return fmt.Sprintf("sent %d via DDC but read back %d", level, readback)
		}
	}

	pkt := nvapi.BuildSetVCP(a.cfg.DDC.SourceAddr, a.cfg.Registers.Volume, level)
	attempts, err := a.bus.Write(pkt, nvapi.WriteOptions{Fast: true, Delay: a.cfg.DDC.BusDelay.D()})
	if err != nil {
		return fmt.Sprintf("DDC unavailable and NVAPI failed: %v", err)
	}

	return fmt.Sprintf("%d over NVAPI raw, %d/%d accepted, unverified",
		level, nvapi.Accepted(attempts), len(attempts))
}
