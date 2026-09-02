package app

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	_cfgmgr32      = syscall.NewLazyDLL("cfgmgr32.dll")
	_idListSizeFn  = _cfgmgr32.NewProc("CM_Get_Device_ID_List_SizeW")
	_idListFn      = _cfgmgr32.NewProc("CM_Get_Device_ID_ListW")
	_filterPresent = uintptr(0x00000100) // CM_GETIDLIST_FILTER_PRESENT
)

// presentDeviceIDs returns the instance ID of every device currently present.
func presentDeviceIDs() ([]string, error) {
	var length uint32
	if r, _, _ := _idListSizeFn.Call(uintptr(unsafe.Pointer(&length)), 0, _filterPresent); r != 0 {
		return nil, fmt.Errorf("CM_Get_Device_ID_List_SizeW: CR=%d", r)
	}
	if length == 0 {
		return nil, nil
	}

	buf := make([]uint16, length)
	if r, _, _ := _idListFn.Call(0, uintptr(unsafe.Pointer(&buf[0])), uintptr(length), _filterPresent); r != 0 {
		return nil, fmt.Errorf("CM_Get_Device_ID_ListW: CR=%d", r)
	}
	return splitMultiSz(buf), nil
}

// splitMultiSz walks a double-null-terminated UTF-16 string list.
func splitMultiSz(b []uint16) []string {
	var out []string
	start := 0

	for i := range b {
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

// deviceMatcher tests device IDs against a set of substrings.
//
// The needles are folded once at construction. Folding them inside the scan
// meant re-uppercasing every match string for every enumerated device --
// around a thousand redundant allocations per poll, which showed up as most
// of a percent of a CPU core in a process that is supposed to be idle.
type deviceMatcher struct{ needles []string }

func newDeviceMatcher(match []string) deviceMatcher {
	needles := make([]string, 0, len(match))
	for _, m := range match {
		if m = strings.TrimSpace(m); m != "" {
			needles = append(needles, strings.ToUpper(m))
		}
	}
	return deviceMatcher{needles: needles}
}

func (d deviceMatcher) empty() bool { return len(d.needles) == 0 }

// find reports whether any watched signature is enumerated, and which device
// matched.
func (d deviceMatcher) find() (bool, string, error) {
	ids, err := presentDeviceIDs()
	if err != nil {
		return false, "", err
	}

	for _, id := range ids {
		upper := strings.ToUpper(id)
		for _, needle := range d.needles {
			if strings.Contains(upper, needle) {
				return true, id, nil
			}
		}
	}
	return false, "", nil
}

// Devices lists present device instance IDs, optionally filtered. Use it to
// find match strings for the watch configuration.
func (a *App) Devices(args []string) error {
	if len(args) > 1 {
		return errors.New("usage: devices [substring]")
	}

	ids, err := presentDeviceIDs()
	if err != nil {
		return err
	}

	var filter string
	if len(args) == 1 {
		filter = strings.ToUpper(args[0])
	}

	var shown int
	for _, id := range ids {
		if filter != "" && !strings.Contains(strings.ToUpper(id), filter) {
			continue
		}
		a.println(id)
		shown++
	}

	a.printf("\n%d of %d present devices shown\n", shown, len(ids))
	return nil
}
