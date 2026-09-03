// Package nvapi issues raw I2C writes through NVIDIA's NVAPI, bypassing the
// Windows DDC/CI stack entirely.
//
// This exists because dxva2.dll hardcodes the DDC source address 0x51 with no
// override, and some monitors -- recent LG panels in particular -- moved
// input selection onto a service sidechannel that only answers at address
// 0x50. NvAPI_I2CWrite puts bytes on the physical bus with no OS-level DDC
// wrapping, so the packet can be built by hand with the address the panel
// actually wants.
//
// Requires Windows x64 and an NVIDIA GPU driving the display. That channel
// never acknowledges: a successful write means the bus accepted the bytes,
// not that the monitor acted on them.
package nvapi

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/klaidliadon/deskmux/vcp"
)

const (
	_maxPhysicalGPUs = 64
	_statusOK        = 0
)

// NVAPI exports exactly one usable symbol; everything else is fetched by
// numeric function ID through it.
const (
	_idInitialize          uint32 = 0x0150E828
	_idEnumPhysicalGPUs    uint32 = 0xE5AC921F
	_idGetConnectedOutputs uint32 = 0x1730BFC9
	_idI2CWrite            uint32 = 0xE812EB07
	_idGetErrorMessage     uint32 = 0x6C2D048C
)

// i2cInfo mirrors NV_I2C_INFO_V3. The explicit padding is load-bearing: get
// it wrong and NVAPI rejects the struct or writes garbage.
type i2cInfo struct {
	Version         uint32
	DisplayMask     uint32
	IsDDCPort       uint8
	I2CDevAddress   uint8
	_               [6]uint8
	PbI2CRegAddress uintptr
	RegAddrSize     uint32
	_               uint32
	PbData          uintptr
	CbSize          uint32
	I2CSpeed        uint32
	I2CSpeedKhz     uint32
	PortID          uint8
	_               [3]uint8
	IsPortIDSet     uint32
}

// Client holds the resolved NVAPI entry points and enumerated GPUs.
type Client struct {
	initialize  uintptr
	enumGPUs    uintptr
	getOutputs  uintptr
	i2cWrite    uintptr
	getErrorMsg uintptr
	gpus        []uintptr
}

// NVAPI initialisation is process-wide and idempotent, and LoadLibrary leaks
// a module reference per call, so resolve once and reuse.
var _load = sync.OnceValues(newClient)

// Load resolves NVAPI and enumerates GPUs. Safe to call repeatedly; the work
// happens once.
func Load() (*Client, error) { return _load() }

func newClient() (*Client, error) {
	lib, err := syscall.LoadLibrary("nvapi64.dll")
	if err != nil {
		return nil, fmt.Errorf("load nvapi64.dll (NVIDIA driver installed?): %w", err)
	}

	var queryInterface uintptr
	for _, name := range []string{"nvapi_QueryInterface", "nvapi64_QueryInterface"} {
		if addr, err := syscall.GetProcAddress(lib, name); err == nil && addr != 0 {
			queryInterface = addr
			break
		}
	}
	if queryInterface == 0 {
		return nil, fmt.Errorf("nvapi_QueryInterface not exported by nvapi64.dll")
	}

	resolve := func(id uint32) (uintptr, error) {
		p, _, _ := syscall.SyscallN(queryInterface, uintptr(id))
		if p == 0 {
			return 0, fmt.Errorf("QueryInterface(0x%08X) returned NULL", id)
		}
		return p, nil
	}

	c := &Client{}
	entries := []struct {
		id  uint32
		dst *uintptr
	}{
		{_idInitialize, &c.initialize},
		{_idEnumPhysicalGPUs, &c.enumGPUs},
		{_idGetConnectedOutputs, &c.getOutputs},
		{_idI2CWrite, &c.i2cWrite},
		{_idGetErrorMessage, &c.getErrorMsg},
	}
	for _, e := range entries {
		p, err := resolve(e.id)
		if err != nil {
			return nil, err
		}
		*e.dst = p
	}

	if st, _, _ := syscall.SyscallN(c.initialize); int32(uint32(st)) != _statusOK {
		return nil, fmt.Errorf("NvAPI_Initialize: %s", c.status(st))
	}

	gpus := make([]uintptr, _maxPhysicalGPUs)
	var count uint32
	st, _, _ := syscall.SyscallN(c.enumGPUs,
		uintptr(unsafe.Pointer(&gpus[0])), uintptr(unsafe.Pointer(&count)))
	if int32(uint32(st)) != _statusOK {
		return nil, fmt.Errorf("NvAPI_EnumPhysicalGPUs: %s", c.status(st))
	}

	c.gpus = gpus[:count]
	if len(c.gpus) == 0 {
		return nil, fmt.Errorf("no NVIDIA GPUs found")
	}
	return c, nil
}

func (c *Client) status(st uintptr) string {
	code := int32(uint32(st))
	if c.getErrorMsg != 0 {
		var buf [64]byte
		syscall.SyscallN(c.getErrorMsg, uintptr(uint32(code)), uintptr(unsafe.Pointer(&buf[0])))
		if s := cString(buf[:]); s != "" {
			return fmt.Sprintf("%s (%d)", s, code)
		}
	}
	return fmt.Sprintf("status %d", code)
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// BuildSetVCP assembles a DDC/CI "Set VCP Feature" packet:
//
//	[source_addr, 0x84, 0x03, vcp_code, value_hi, value_lo, checksum]
//
// 0x84 is the length byte (0x80 | 4 data bytes) and 0x03 the Set VCP opcode.
// The destination address is not in the buffer but is folded into the
// checksum.
func BuildSetVCP(source vcp.SourceAddr, code vcp.Code, value vcp.Level) []byte {
	pkt := make([]byte, 7)
	copy(pkt, []byte{byte(source), 0x84, 0x03, byte(code), byte(value >> 8), byte(value)})

	checksum := vcp.DeviceAddr
	for _, b := range pkt[:6] {
		checksum ^= b
	}
	pkt[6] = checksum

	return pkt
}

// Attempt records one (gpu, mask, port) write.
type Attempt struct {
	GPU     int
	Mask    uint32
	Port    int
	HasPort bool
	OK      bool
	Status  string
}

// WriteOptions tunes how widely Write casts its net.
type WriteOptions struct {
	// Fast skips the combined-mask write and the port sweep, sending only
	// one write per connected output. On hardware where that is sufficient
	// it turns a ~1500ms call into ~140ms.
	Fast bool

	// Delay is the pause between writes. DDC wants roughly 40ms between
	// transactions to the same display; going faster risks wedging panels
	// with fragile DDC engines.
	Delay time.Duration

	// OnGPUError, if set, receives per-GPU enumeration failures.
	OnGPUError func(gpu int, err error)
}

// Write fires the packet at connected outputs and reports every attempt.
//
// The breadth is not laziness: this channel never acknowledges, so there is
// no way to discover which (mask, port) pair addresses the target monitor.
// Firing at all plausible combinations and reporting which the bus accepted
// is the only available strategy.
func (c *Client) Write(pkt []byte, opts WriteOptions) []Attempt {
	if opts.Delay <= 0 {
		opts.Delay = 40 * time.Millisecond
	}

	attempts := make([]Attempt, 0, 16)

	try := func(gpuIndex int, gpu uintptr, mask uint32, port int, hasPort bool) {
		info := i2cInfo{
			DisplayMask:   mask,
			IsDDCPort:     1,
			I2CDevAddress: vcp.DeviceAddr,
			PbData:        uintptr(unsafe.Pointer(&pkt[0])),
			CbSize:        uint32(len(pkt)),
			I2CSpeed:      0xFFFF, // deprecated field; must be 0xFFFF for v2+
			I2CSpeedKhz:   4,      // NVAPI_I2C_SPEED_100KHZ
		}
		info.Version = (3 << 16) | uint32(unsafe.Sizeof(info))
		if hasPort {
			info.PortID = uint8(port)
			info.IsPortIDSet = 1
		}

		st, _, _ := syscall.SyscallN(c.i2cWrite, gpu, uintptr(unsafe.Pointer(&info)))
		runtime.KeepAlive(pkt)
		runtime.KeepAlive(&info)

		attempts = append(attempts, Attempt{
			GPU:     gpuIndex,
			Mask:    mask,
			Port:    port,
			HasPort: hasPort,
			OK:      int32(uint32(st)) == _statusOK,
			Status:  c.status(st),
		})
		time.Sleep(opts.Delay)
	}

	for i, gpu := range c.gpus {
		mask, err := c.connectedOutputs(gpu)
		if err != nil {
			if opts.OnGPUError != nil {
				opts.OnGPUError(i, err)
			}
			continue
		}
		if mask == 0 {
			continue
		}

		// The combined mask is rejected on at least the LG 45GX950A -- it is
		// the single failure behind a familiar "18/19 accepted". Only writes
		// addressed to an individual output bit land there.
		if !opts.Fast {
			try(i, gpu, mask, 0, false)
		}

		for bit := range 32 {
			one := uint32(1) << bit
			if mask&one == 0 {
				continue
			}
			try(i, gpu, one, 0, false)
			if opts.Fast {
				continue
			}
			for port := range 8 {
				try(i, gpu, one, port, true)
			}
		}
	}
	return attempts
}

func (c *Client) connectedOutputs(gpu uintptr) (uint32, error) {
	var mask uint32
	st, _, _ := syscall.SyscallN(c.getOutputs, gpu, uintptr(unsafe.Pointer(&mask)))
	if int32(uint32(st)) != _statusOK {
		return 0, fmt.Errorf("NvAPI_GPU_GetConnectedOutputs: %s", c.status(st))
	}
	return mask, nil
}
