package main

// NVAPI raw-I2C backend.
//
// Why this exists: LG moved input selection off the standard DDC register
// (VCP 0x60) onto a service/manufacturer sidechannel -- proprietary VCP 0xF4
// delivered with the DDC *source address* set to 0x50 ("DDC2AB") instead of
// the standard 0x51. Windows' dxva2.dll hardcodes 0x51 and offers no
// override, so no conventional tool can reach it.
//
// NvAPI_I2CWrite puts raw bytes on the physical I2C bus with no OS-level
// DDC/CI wrapping, so we build the packet by hand.
//
// Requires an NVIDIA GPU driving the monitor. This channel never
// acknowledges, so there is no read-back: success here means "the bus write
// returned OK", and the only real confirmation is the screen changing.

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

const (
	ddcDeviceAddr byte = 0x6E // DDC/CI destination (I2C 0x37 << 1)

	maxPhysicalGPUs = 64
	nvapiOK         = 0
)

// NVAPI exports exactly one usable symbol; everything else is fetched by
// numeric function ID through it.
const (
	idInitialize          uint32 = 0x0150E828
	idEnumPhysicalGPUs    uint32 = 0xE5AC921F
	idGetConnectedOutputs uint32 = 0x1730BFC9
	idI2CWrite            uint32 = 0xE812EB07
	idGetErrorMessage     uint32 = 0x6C2D048C
)

// nvI2CInfo mirrors NV_I2C_INFO_V3. The explicit padding is load-bearing:
// get it wrong and NVAPI rejects the struct or writes garbage.
type nvI2CInfo struct {
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

type nvapiClient struct {
	initialize  uintptr
	enumGPUs    uintptr
	getOutputs  uintptr
	i2cWrite    uintptr
	getErrorMsg uintptr
	gpus        []uintptr
}

// BuildSetVCP assembles a DDC/CI "Set VCP Feature" packet.
//
//	[source_addr, 0x84, 0x03, vcp_code, value_hi, value_lo, checksum]
//
// 0x84 is the length byte (0x80 | 4 data bytes), 0x03 the Set VCP opcode.
// The destination address 0x6E is not in the buffer -- it goes in
// i2cDevAddress -- but it IS folded into the checksum.
func BuildSetVCP(sourceAddr, vcpCode byte, value uint16) []byte {
	pkt := []byte{sourceAddr, 0x84, 0x03, vcpCode, byte(value >> 8), byte(value)}
	ck := ddcDeviceAddr
	for _, b := range pkt {
		ck ^= b
	}
	return append(pkt, ck)
}

// NVAPI init is process-wide and idempotent, and LoadLibrary leaks a module
// reference on every call, so resolve once and reuse. This also takes a
// noticeable chunk off every `input` invocation.
var (
	nvOnce   sync.Once
	nvClient *nvapiClient
	nvErr    error
)

func loadNVAPI() (*nvapiClient, error) {
	nvOnce.Do(func() { nvClient, nvErr = initNVAPI() })
	return nvClient, nvErr
}

func initNVAPI() (*nvapiClient, error) {
	lib, err := syscall.LoadLibrary("nvapi64.dll")
	if err != nil {
		return nil, fmt.Errorf("nvapi64.dll not loadable (NVIDIA driver installed?): %w", err)
	}

	var qi uintptr
	for _, name := range []string{"nvapi_QueryInterface", "nvapi64_QueryInterface"} {
		if addr, err := syscall.GetProcAddress(lib, name); err == nil && addr != 0 {
			qi = addr
			break
		}
	}
	if qi == 0 {
		return nil, fmt.Errorf("nvapi_QueryInterface not exported by nvapi64.dll")
	}

	resolve := func(id uint32) (uintptr, error) {
		p, _, _ := syscall.SyscallN(qi, uintptr(id))
		if p == 0 {
			return 0, fmt.Errorf("QueryInterface(0x%08X) returned NULL", id)
		}
		return p, nil
	}

	c := &nvapiClient{}
	for _, e := range []struct {
		id  uint32
		dst *uintptr
	}{
		{idInitialize, &c.initialize},
		{idEnumPhysicalGPUs, &c.enumGPUs},
		{idGetConnectedOutputs, &c.getOutputs},
		{idI2CWrite, &c.i2cWrite},
		{idGetErrorMessage, &c.getErrorMsg},
	} {
		p, err := resolve(e.id)
		if err != nil {
			return nil, err
		}
		*e.dst = p
	}

	if st, _, _ := syscall.SyscallN(c.initialize); int32(uint32(st)) != nvapiOK {
		return nil, fmt.Errorf("NvAPI_Initialize failed: %s", c.status(st))
	}

	gpus := make([]uintptr, maxPhysicalGPUs)
	var count uint32
	st, _, _ := syscall.SyscallN(c.enumGPUs,
		uintptr(unsafe.Pointer(&gpus[0])), uintptr(unsafe.Pointer(&count)))
	if int32(uint32(st)) != nvapiOK {
		return nil, fmt.Errorf("NvAPI_EnumPhysicalGPUs failed: %s", c.status(st))
	}
	c.gpus = gpus[:count]
	if len(c.gpus) == 0 {
		return nil, fmt.Errorf("no NVIDIA GPUs found")
	}
	return c, nil
}

func (c *nvapiClient) status(st uintptr) string {
	code := int32(uint32(st))
	if c.getErrorMsg != 0 {
		var buf [64]byte
		syscall.SyscallN(c.getErrorMsg, uintptr(uint32(code)), uintptr(unsafe.Pointer(&buf[0])))
		if s := cstr(buf[:]); s != "" {
			return fmt.Sprintf("%s (%d)", s, code)
		}
	}
	return fmt.Sprintf("status %d", code)
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// connectedOutputs returns the display mask for one GPU.
func (c *nvapiClient) connectedOutputs(gpu uintptr) (uint32, error) {
	var mask uint32
	st, _, _ := syscall.SyscallN(c.getOutputs, gpu, uintptr(unsafe.Pointer(&mask)))
	if int32(uint32(st)) != nvapiOK {
		return 0, fmt.Errorf("NvAPI_GPU_GetConnectedOutputs failed: %s", c.status(st))
	}
	return mask, nil
}

// I2CAttempt records one (gpu, mask, port) write.
type I2CAttempt struct {
	GPU     int
	Mask    uint32
	Port    int
	HasPort bool
	OK      bool
	Status  string
}

// WritePacket fires the packet at every (displayMask, portId) combination.
//
// This brute force is not laziness: the sidechannel never acknowledges, so
// there is no way to discover which pair corresponds to the target monitor.
// Firing at all of them and reporting any success is the documented approach.
func (c *nvapiClient) WritePacket(pkt []byte, dryRun bool, verbose bool) []I2CAttempt {
	var attempts []I2CAttempt

	try := func(gpuIdx int, gpu uintptr, mask uint32, port int, hasPort bool) {
		a := I2CAttempt{GPU: gpuIdx, Mask: mask, Port: port, HasPort: hasPort}
		if dryRun {
			a.Status = "dry-run"
			attempts = append(attempts, a)
			return
		}
		info := nvI2CInfo{
			DisplayMask:   mask,
			IsDDCPort:     1,
			I2CDevAddress: ddcDeviceAddr,
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
		a.OK = int32(uint32(st)) == nvapiOK
		a.Status = c.status(st)
		attempts = append(attempts, a)
		sleepBus()
	}

	for gi, gpu := range c.gpus {
		mask, err := c.connectedOutputs(gpu)
		if err != nil {
			if verbose {
				logf("  gpu %d: %v\n", gi, err)
			}
			continue
		}
		if mask == 0 {
			continue
		}
		// The combined mask is measurably NOT accepted on this hardware --
		// it is the single rejected attempt behind the familiar "18/19
		// accepted". Only writes addressed to an individual output bit land,
		// so -fast sends just those and skips the port sweep.
		if !*flagFast {
			try(gi, gpu, mask, 0, false)
		}

		for bit := 0; bit < 32; bit++ {
			one := uint32(1) << bit
			if mask&one == 0 {
				continue
			}
			try(gi, gpu, one, 0, false)
			if *flagFast {
				continue
			}
			for port := 0; port < 8; port++ {
				try(gi, gpu, one, port, true)
			}
		}
	}
	return attempts
}
