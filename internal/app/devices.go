package app

import (
	"errors"
	"fmt"
	"slices"
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
//
// An empty string ends the list, which is what the second NUL of the pair
// looks like once the first has been consumed as a terminator. A buffer with
// no terminator at all ends it too, since there is no complete string left.
func splitMultiSz(b []uint16) []string {
	var out []string

	for len(b) > 0 {
		end := slices.Index(b, 0)
		if end <= 0 {
			break
		}
		out = append(out, syscall.UTF16ToString(b[:end]))
		b = b[end+1:]
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

	total := len(ids)
	if len(args) == 1 {
		filter := strings.ToUpper(args[0])
		ids = slices.DeleteFunc(ids, func(id string) bool {
			return !strings.Contains(strings.ToUpper(id), filter)
		})
	}

	for _, id := range ids {
		a.println(id)
	}

	a.printf("\n%d of %d present devices shown\n", len(ids), total)
	return nil
}
