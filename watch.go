package main

// Dock watcher.
//
// Watches for the OWC Thunderbolt 3 dock arriving or leaving and applies a
// profile to the monitor for whichever machine is about to use it.
//
// The asymmetry is deliberate:
//
//   - Dock CONNECTS -> we are docking at the Blade. Grab the panel onto USB-C
//     and raise the monitor volume to a comfortable Windows level.
//   - Dock DISCONNECTS -> the dock is on its way to a Mac. Do NOT switch the
//     input; Auto Input Switch hands the panel over by itself once the Mac
//     presents DisplayPort. Instead drop VCP 0x62 to a Mac-safe level.
//
// That last point is what makes a high Windows level safe at all. 0x62 is a
// monitor-global setting, and macOS cannot attenuate DP audio, so a monitor
// left loud would hand the Mac full blast. Windows keeps its direct USB-C
// link after the dock leaves, so it can still write 0x62 at that moment --
// which is exactly the window this watcher uses.

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	cfgmgr32       = syscall.NewLazyDLL("cfgmgr32.dll")
	procIDListSize = cfgmgr32.NewProc("CM_Get_Device_ID_List_SizeW")
	procIDList     = cfgmgr32.NewProc("CM_Get_Device_ID_ListW")
)

const cmFilterPresent = 0x00000100 // CM_GETIDLIST_FILTER_PRESENT

// presentDeviceIDs returns the instance ID of every device currently present.
func presentDeviceIDs() ([]string, error) {
	var n uint32
	r, _, _ := procIDListSize.Call(uintptr(unsafe.Pointer(&n)), 0, uintptr(cmFilterPresent))
	if r != 0 {
		return nil, fmt.Errorf("CM_Get_Device_ID_List_SizeW failed: CR=%d", r)
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]uint16, n)
	r, _, _ = procIDList.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(cmFilterPresent))
	if r != 0 {
		return nil, fmt.Errorf("CM_Get_Device_ID_ListW failed: CR=%d", r)
	}
	return splitMultiSz(buf), nil
}

// splitMultiSz walks a double-null-terminated UTF-16 string list.
func splitMultiSz(b []uint16) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != 0 {
			continue
		}
		if i > start {
			out = append(out, syscall.UTF16ToString(b[start:i]))
		}
		start = i + 1
		if i+1 < len(b) && b[i+1] == 0 {
			break
		}
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

// dockPresent reports whether any watched signature is currently enumerated.
func dockPresent(matches []string) (bool, string, error) {
	ids, err := presentDeviceIDs()
	if err != nil {
		return false, "", err
	}
	for _, id := range ids {
		up := strings.ToUpper(id)
		for _, m := range matches {
			if strings.Contains(up, m) {
				return true, id, nil
			}
		}
	}
	return false, "", nil
}

func cmdDevices(args []string) error {
	ids, err := presentDeviceIDs()
	if err != nil {
		return err
	}
	filter := ""
	if len(args) == 1 {
		filter = strings.ToUpper(args[0])
	}
	n := 0
	for _, id := range ids {
		if filter != "" && !strings.Contains(strings.ToUpper(id), filter) {
			continue
		}
		logln(id)
		n++
	}
	fmt.Fprintf(os.Stderr, "\n%d of %d present devices shown\n", n, len(ids))
	return nil
}

func ts() string { return time.Now().Format("15:04:05") }

// applyProfile runs one side of the handoff.
func applyProfile(label, input string, vol int) {
	logf("[%s] %s\n", ts(), label)

	if vol >= 0 {
		logf("  volume: %s\n", setVolumeBestEffort(uint32(vol)))
	}

	if input == "" {
		logf("  input : unchanged (the monitor's Auto Input Switch handles the handover)\n")
		return
	}
	val, err := resolveInput(input)
	if err != nil {
		logf("  input : %v\n", err)
		return
	}
	if err := sendRaw(srcLGInput, vcpInputLG, val, fmt.Sprintf("  input -> %s", input)); err != nil {
		logf("  input : %v\n", err)
	}
}

func cmdWatch(args []string) error {
	matches := splitList(*flagMatch)
	if len(matches) == 0 {
		return fmt.Errorf("-match must not be empty")
	}

	logf("watching for dock signature: %s\n", strings.Join(matches, ", "))
	logf("  on connect    : input=%s vol=%d\n", *flagDockInput, *flagDockVol)
	if *flagUndockInput == "" {
		logf("  on disconnect : vol=%d, input unchanged\n", *flagUndockVol)
	} else {
		logf("  on disconnect : input=%s vol=%d\n", *flagUndockInput, *flagUndockVol)
	}
	logf("  poll every    : %s\n\n", *flagPoll)

	state, id, err := dockPresent(matches)
	if err != nil {
		return err
	}
	if state {
		logf("[%s] dock is present at startup (%s) -- not firing\n", ts(), id)
	} else {
		logf("[%s] dock is absent at startup -- not firing\n", ts())
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	tick := time.NewTicker(*flagPoll)
	defer tick.Stop()

	// Device arrival fires in bursts as each dock component enumerates, so
	// require the reading to hold for two consecutive polls before acting.
	pending := state
	stable := 0

	for {
		select {
		case <-stop:
			logf("\n[%s] stopped\n", ts())
			return nil
		case <-tick.C:
			now, nowID, err := dockPresent(matches)
			if err != nil {
				logf("[%s] scan error: %v\n", ts(), err)
				continue
			}
			if now != pending {
				pending, stable = now, 0
				continue
			}
			if now == state {
				continue
			}
			if stable++; stable < 2 {
				continue
			}
			state = now
			stable = 0
			if now {
				logf("[%s] DOCK CONNECTED (%s)\n", ts(), nowID)
				applyProfile("applying Windows profile", *flagDockInput, *flagDockVol)
			} else {
				logf("[%s] DOCK DISCONNECTED\n", ts())
				applyProfile("applying Mac profile", *flagUndockInput, *flagUndockVol)
				if *flagUndockPower {
					// Last, and only if asked: the panel stops answering DDC
					// once it is off, so nothing can follow this.
					if err := cmdPower([]string{"off"}); err != nil {
						logf("  power : %v\n", err)
					}
				}
			}
			logln()
		}
	}
}
