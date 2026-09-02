package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klaidliadon/lginput/config"
	"github.com/klaidliadon/lginput/ddc"
)

// Probe reads every register this tool knows about and dumps the capabilities
// string. It is the starting point for adapting the config to a new monitor.
func (a *App) Probe() error {
	set, m, err := a.openMonitor()
	if err != nil {
		return err
	}
	defer set.Close()

	a.printf("monitor: %s\n\n", m.Name)
	if caps, err := m.Capabilities(); err == nil {
		a.printf("caps: %s\n\n", caps)
	} else {
		a.log.Warn("capabilities read failed", "err", err)
	}

	r := a.cfg.Registers
	codes := []struct {
		code byte
		name string
	}{
		{r.Brightness, "brightness"},
		{r.Contrast, "contrast"},
		{r.Volume, "volume"},
		{r.Mute, "mute"},
		{r.InputStandard, "input select (standard)"},
		{a.cfg.Inputs.VCP, "input select (configured)"},
		{a.cfg.PBP.VCP, "PBP"},
		{a.cfg.Power.VCP, "power"},
	}

	a.printf("%-6s %-28s %s\n", "code", "name", "read")
	for _, c := range codes {
		v, err := m.GetVCP(c.code)
		if err != nil {
			a.printf("0x%02X   %-28s unsupported\n", c.code, c.name)
			continue
		}
		a.printf("0x%02X   %-28s current=%d max=%d type=%d\n", c.code, c.name, v.Current, v.Max, v.Type)
	}

	a.println("\nnote: a register reading fine says nothing about whether writes land.")
	a.println("      Some panels advertise 0x60 and silently ignore writes to it.")
	return nil
}

// Get reads one VCP code.
func (a *App) Get(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: get <vcp>")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}

	set, m, err := a.openMonitor()
	if err != nil {
		return err
	}
	defer set.Close()

	v, err := m.GetVCP(code)
	if err != nil {
		return err
	}
	a.printf("0x%02X: current=%d (0x%02X) max=%d type=%d\n", code, v.Current, v.Current, v.Max, v.Type)
	return nil
}

// Set writes one VCP code and verifies it by reading back.
func (a *App) Set(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: set <vcp> <value>")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}
	value, err := parseValue(args[1])
	if err != nil {
		return err
	}

	set, m, err := a.openMonitor()
	if err != nil {
		return err
	}
	defer set.Close()

	if a.opts.DryRun {
		a.printf("dry-run: set 0x%02X = %d\n", code, value)
		return nil
	}

	landed, readback, err := m.SetVCPVerified(code, value, a.cfg.DDC.Settle.D())
	if err != nil {
		return err
	}

	a.printf("0x%02X <- %d; read back %d; landed=%v\n", code, value, readback, landed)
	if !landed {
		a.println("the API accepted the write but the monitor did not take it")
	}
	return nil
}

// Level gets or sets a 0..max register such as brightness or volume.
func (a *App) Level(args []string, code byte, label string) error {
	set, m, err := a.openMonitor()
	if err != nil {
		return err
	}
	defer set.Close()

	cur, err := m.GetVCP(code)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		a.printf("%s: %d (max %d)\n", label, cur.Current, cur.Max)
		return nil
	}

	target, err := resolveLevel(args[0], cur)
	if err != nil {
		return err
	}
	if a.opts.DryRun {
		a.printf("dry-run: %s %d -> %d\n", label, cur.Current, target)
		return nil
	}

	landed, readback, err := m.SetVCPVerified(code, target, a.cfg.DDC.Settle.D())
	if err != nil {
		return err
	}
	a.printf("%s: %d -> %d (read back %d, landed=%v)\n", label, cur.Current, target, readback, landed)
	return nil
}

func resolveLevel(arg string, cur ddc.Value) (uint32, error) {
	relative := strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "-")

	n, err := strconv.ParseInt(arg, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("bad level %q (want N, +N or -N)", arg)
	}

	v := n
	if relative {
		v = int64(cur.Current) + n
	}
	return uint32(min(max(v, 0), int64(cur.Max))), nil
}

// Mute writes the mute register. 1 mutes, 2 unmutes, per MCCS.
func (a *App) Mute(value uint32) error {
	set, m, err := a.openMonitor()
	if err != nil {
		return err
	}
	defer set.Close()

	if a.opts.DryRun {
		a.printf("dry-run: mute = %d\n", value)
		return nil
	}

	landed, readback, err := m.SetVCPVerified(a.cfg.Registers.Mute, value, a.cfg.DDC.Settle.D())
	if err != nil {
		return err
	}
	a.printf("mute <- %d; read back %d; landed=%v\n", value, readback, landed)
	return nil
}

// Input switches the monitor's input source.
func (a *App) Input(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: input <%s>", joinSorted(a.cfg.Inputs.Targets))
	}

	value, err := a.cfg.ResolveInput(strings.ToLower(strings.TrimSpace(args[0])))
	if err != nil {
		return err
	}
	return a.sendRaw(a.cfg.Inputs.SourceAddr, a.cfg.Inputs.VCP, value, fmt.Sprintf("input -> %s", args[0]))
}

// Table drives a named-value register such as PBP or power.
//
// Both answer at the standard DDC source address, so they go through the
// normal path first and fall back to raw I2C only if that fails -- which is
// what makes `power off` recoverable when the DDC engine has wedged.
func (a *App) Table(args []string, t config.Table, label string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: %s <%s>", label, joinSorted(t.Modes))
	}

	mode, ok := t.Modes[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("unknown %s mode %q (want %s)", label, args[0], joinSorted(t.Modes))
	}

	a.printf("%s %s: 0x%02X = 0x%02X\n", label, args[0], t.VCP, mode)
	if a.opts.DryRun {
		return nil
	}

	if set, m, err := a.openMonitor(); err == nil {
		defer set.Close()
		if err := m.SetVCP(t.VCP, uint32(mode)); err == nil {
			a.println("  sent via DDC")

			time.Sleep(a.cfg.DDC.Settle.D())
			if v, err := m.GetVCP(t.VCP); err == nil {
				a.printf("  read back: %d (0x%02X)\n", v.Current, v.Current)
			}
			return nil
		}
	}
	return a.sendRaw(t.SourceAddr, t.VCP, mode, fmt.Sprintf("%s %s", label, args[0]))
}

// Raw sends a hand-built packet over NVAPI, the escape hatch for when the
// Windows DDC layer will not answer at all.
func (a *App) Raw(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: raw <vcp> <value>")
	}
	code, err := parseVCP(args[0])
	if err != nil {
		return err
	}
	value, err := parseValue(args[1])
	if err != nil {
		return err
	}
	return a.sendRaw(a.cfg.PBP.SourceAddr, code, uint16(value), "raw")
}

// Config manages the configuration file.
func (a *App) Config(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: config <init|show|path>")
	}

	switch args[0] {
	case "init":
		path := defaultConfigPath()
		if len(args) > 1 {
			path = args[1]
		}
		if err := config.WriteTemplate(path); err != nil {
			return err
		}
		a.printf("wrote %s\n", path)
		return nil

	case "show":
		a.println(config.Template)
		return nil

	case "path":
		for _, p := range config.SearchPaths() {
			marker := " "
			if _, err := os.Stat(p); err == nil {
				marker = "*"
			}
			a.printf("%s %s\n", marker, p)
		}
		a.printf("\n(* = exists)\n")
		return nil

	default:
		return fmt.Errorf("unknown config subcommand %q (want init, show or path)", args[0])
	}
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "lginput", "config.yaml")
	}
	return "lginput.yaml"
}

func joinSorted[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " | ")
}
