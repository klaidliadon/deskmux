# lginput

Monitor control for the **LG UltraGear 45GX950A** on Windows — including input
switching, which no other Windows tool can do on this panel.

Single static `.exe`, no dependencies, no runtime, no installer.

## The problem

This monitor advertises the standard DDC/CI input-source register (VCP `0x60`)
in its capabilities string, then silently discards every write to it:

```
caps: ... vcp(... 60(11 12 0F 10 ) ...)      <- claims to support it
SetVCPFeature(0x60, 17) -> ok=True            <- write accepted
0x60 reads 15, 15, 15, 15, 15                 <- nothing happened
```

It isn't a permissions problem, a conflicting app, or a bad cable — writes to
`0x10` (brightness) and `0x62` (volume) land fine from the same process,
unelevated, seconds apart.

LG moved input selection onto a service sidechannel: proprietary VCP **`0xF4`**,
sent with the DDC **source address `0x50`** ("DDC2AB") instead of the standard
`0x51`. Windows' `dxva2.dll` hardcodes `0x51` with no override, which is why
Twinkle Tray, ControlMyMonitor and everything else fail here — they all issue
the same `SetVCPFeature` call.

`lginput` bypasses that using NVIDIA's `NvAPI_I2CWrite`, which puts raw bytes
on the physical I2C bus with no OS-level DDC/CI wrapping, so the packet can be
built by hand with the right source address.

```
[source_addr, 0x84, 0x03, vcp_code, value_hi, value_lo, checksum]
 ^^^^^^^^^^^                                            checksum = 0x6E XOR all
```

| Input | Value | Packet |
|---|---|---|
| HDMI 1 | `0x90` | `50 84 03 F4 00 90 DD` |
| HDMI 2 | `0x91` | `50 84 03 F4 00 91 DC` |
| DisplayPort | `0xD0` | `50 84 03 F4 00 D0 9D` |
| USB-C | `0xD1` | `50 84 03 F4 00 D1 9C` |

## Requirements

- Windows 10/11 x64
- **An NVIDIA GPU driving the monitor** — required for `input` only. Everything
  else uses the standard API and works on any GPU.
- No admin rights needed.

## Commands

```
lginput [flags] <command> [args]

list                    enumerate monitors and show key register values
caps                    dump the raw DDC capabilities string
probe                   read every interesting register
get <vcp>               read one VCP code             (get 0x10)
set <vcp> <value>       write one VCP code, verified  (set 0x62 30)
brightness [v|+v|-v]    brightness (0x10)
volume     [v|+v|-v]    volume     (0x62)
mute | unmute           audio mute (0x8D)
input <target>          hdmi1 | hdmi2 | dp | usb-c    (0xF4 @ 0x50, via NVAPI)
pbp <off|50|66>         picture-by-picture (0xD7 @ 0x51)
power <on|off>          DPMS (0xD6)
raw <vcp> <value>       hand-built packet over NVAPI raw I2C
watch                   apply a profile when a dock connects/disconnects
volumekeys              volume keys drive the monitor instead of Windows
devices [substr]        list present device IDs
```

Useful flags: `-n` dry run, `-v` verbose, `-fast` (see below), `-log <file>`.

### `volumekeys`

Makes Windows behave like macOS: the Windows endpoint is pinned at 100% and the
volume keys drive the monitor's own hardware volume. Useful when two machines
share the panel, because `0x62` is then the single, machine-independent volume
control.

```bash
lginput -step 1 volumekeys
```

Only active while the monitor is the default playback device — on headphones or
laptop speakers the keys pass straight through to Windows, with the native
flyout and normal per-endpoint volume. Match with `-audio-match`.

### `watch`

Watches for a dock and applies an input profile per machine:

```bash
lginput -undock-input dp watch
```

Find a `-match` string for your own dock with `lginput devices <substr>`.

## Known issues and caveats

- **Tested on exactly one monitor**, a 45GX950A (`GSM 9E9F`, DDC model `WK95U`).
  The `0xF4` values come from the ddcutil wiki's LG table and should apply to
  other recent LG models, but nothing here is verified beyond this panel.
- **The sidechannel never acknowledges.** `input` reports how many bus writes
  were accepted, which is not the same as the monitor obeying. The only real
  confirmation is the screen changing. `0x60` also *misreports* — it reads
  `15` (DisplayPort) while the live input is USB-C — so it cannot be used to
  verify either.
- **`-fast` needs per-machine verification.** By default `input` sweeps every
  display mask and port because there is no way to discover which pair is the
  right one. `-fast` sends only the per-output-bit writes: ~140ms instead of
  ~1500ms. On the development machine the *combined* mask write is always
  rejected and only the per-bit writes land, but confirm on yours before
  relying on it.
- **The panel's DDC engine wedges.** After certain writes it stops answering
  and every read fails with `ERROR_GRAPHICS_I2C_ERROR_RECEIVING_DATA`. It
  recovers on a monitor power cycle. The NVAPI path keeps working throughout,
  since it never touches the Windows DDC layer — so `input` still works when
  `volume` and `brightness` do not.
- **`power off` is one-way.** Once off, the panel stops answering DDC, so
  `power on` will usually fail; wake it with a signal or the joystick.
- **`go vet` reports 4 `possible misuse of unsafe.Pointer`.** These are
  inherent to COM vtable dispatch and Win32 hook callbacks, where the pointers
  are OS-owned native memory the Go GC never manages.
- `probe` takes ~2s, dominated by the capabilities string read. It is a
  diagnostic, not a hot path.

## Build

```bash
go build -o lginput.exe .
```

Go 1.25+. No module dependencies.

To run a daemon without a console window, build with:

```bash
go build -ldflags -H=windowsgui -o lginputw.exe .
```

and use `-log` for output.

## Credits

The `0xF4` / `0x50` mechanism is documented in the
[ddcutil wiki](https://github.com/rockowitz/ddcutil/wiki/Switching-input-source-on-LG-monitors),
and [meer-cha/lg-input-switch](https://github.com/meer-cha/lg-input-switch)
implements the same approach in Python.

## Licence

MIT — see [LICENSE](LICENSE).
