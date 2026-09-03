# deskmux

DDC/CI monitor control for Windows — including **input switching on LG panels
that no other Windows tool can switch**.

Single static `.exe`. One dependency (a YAML parser). No installer, no admin
rights, no runtime.

## The problem

Some monitors advertise the standard DDC/CI input-source register, VCP `0x60`,
and then silently discard every write to it:

```
caps: ... vcp(... 60(11 12 0F 10 ) ...)     <- claims to support it
SetVCPFeature(0x60, 17) -> ok=True           <- write accepted
0x60 reads 15, 15, 15, 15, 15                <- nothing happened
```

That is not a permissions problem, a conflicting app or a bad cable. On the LG
UltraGear 45GX950A this was measured directly: writes to `0x10` (brightness)
and `0x62` (volume) land from the same process, unelevated, seconds apart. Only
`0x60` is ignored — and it *misreports* too, reading `15` (DisplayPort) while
the live input is USB-C.

LG moved input selection to a service sidechannel: proprietary VCP **`0xF4`**,
sent with the DDC **source address `0x50`** ("DDC2AB") rather than the standard
`0x51`. Windows' `dxva2.dll` hardcodes `0x51` with no override, which is why
Twinkle Tray, ControlMyMonitor and every other conventional tool fail here —
they all issue the same `SetVCPFeature` call.

`deskmux` builds the packet by hand and puts it on the bus with NVIDIA's
`NvAPI_I2CWrite`, which does no OS-level DDC wrapping:

```
[source_addr, 0x84, 0x03, vcp_code, value_hi, value_lo, checksum]
 ^^^^^^^^^^^                              checksum = 0x6E XOR every byte above
 the field Windows will not let you set
```

## Requirements

- Windows 10/11 x64
- **An NVIDIA GPU driving the monitor** — for `input` only. Everything else
  uses the standard API and works on any GPU.
- No admin rights.

## Install

With [scoop](https://scoop.sh):

```bash
scoop bucket add deskmux https://github.com/klaidliadon/deskmux
scoop install deskmux
```

Or download a zip from [releases](https://github.com/klaidliadon/deskmux/releases).
The binaries are unsigned, so SmartScreen will warn on first run.

From source:

```bash
go build -o deskmux.exe ./cmd/deskmux
```

Go 1.25+. For a daemon with no console window:

```bash
go build -ldflags -H=windowsgui -o deskmuxw.exe ./cmd/deskmux
```

## Quick start

```bash
deskmux config init      # write a documented config
deskmux probe            # see what your monitor exposes
deskmux input usb-c      # switch
```

## Commands

**Daemons**

| | |
|---|---|
| `watch` | Apply a profile when a dock appears or disappears |
| `volumekeys` | Volume keys drive the monitor's own volume |

**Control**

| | |
|---|---|
| `input <target>` | Switch input source |
| `volume [v\|+v\|-v]` | Monitor volume |
| `brightness [v\|+v\|-v]` | Monitor brightness |
| `mute` / `unmute` | Monitor audio mute |
| `pbp <mode>` | Picture-by-picture |
| `power <on\|off>` | Monitor power |

**Diagnostics**

| | |
|---|---|
| `probe` | Every configured register, plus the capabilities string |
| `get <vcp>` / `set <vcp> <value>` | Read / write any register, verified |
| `raw <vcp> <value> [source]` | Hand-built packet over raw I2C, bypassing the Windows DDC layer |
| `devices [substr]` | Present device IDs, for finding `watch` match strings |

Flags: `-config`, `-m`, `-n` (dry run), `-v`, `-fast`, `-log`, `-log-level`.

### `volumekeys`

Makes Windows behave like macOS: the Windows endpoint is pinned at 100% and the
volume keys drive the monitor's hardware volume instead. Useful when two
machines share a panel, because the monitor's own volume register is then the
single, machine-independent control.

It is active **only while the monitor is the default playback device**. On
headphones the keys pass straight through to Windows with the native flyout and
normal per-endpoint volume. Windows tracks volume per device, so pinning the
monitor's endpoint never touches your headphones' level.

### `watch`

```bash
deskmux watch
```

The two directions are deliberately asymmetric. On connect it grabs the panel.
On disconnect the dock is on its way to another machine, so it pushes the panel
to that machine's input *before* it arrives.

That asymmetry is load-bearing: **a monitor will not abandon a live signal just
because a new one appears**, so the panel's own "auto input switch" setting
cannot perform this handover while the first machine is still driving it.
Pushing early works because a panel will happily sit on an input that has no
signal yet.

## Configuration

`deskmux config init` writes a commented file to
`%APPDATA%\deskmux\config.yaml`. Everything hardware-specific lives there, so
adapting to a different monitor is an edit, not a recompile.

The section that matters:

```yaml
inputs:
  vcp: 0xF4          # 0x60 on most monitors; 0xF4 on recent LG
  source_addr: 0x50  # 0x51 on most monitors; 0x50 for the LG sidechannel
  targets:
    hdmi1: 0x90
    hdmi2: 0x91
    dp: 0xD0
    usb-c: 0xD1
```

**Adapting to another monitor:** run `probe` to see which registers respond,
try `set 0x60 17` to check whether the standard path works, and if it does not,
consult the [ddcutil LG value table][ddcutil] for your model. `raw` lets you
try a code and source address without editing anything.

A partial config overlays onto the defaults, so you only write what differs.
Maps replace rather than merge: if you define `inputs.targets` at all, yours
are the only ones — the built-ins do not survive alongside, so a monitor whose
values differ does not silently keep the wrong ones. Unknown keys are an error
rather than being ignored, so a typo tells you instead of quietly leaving the
default in place.

## Known issues and caveats

- **Verified on exactly one monitor**, an LG 45GX950A (`GSM 9E9F`, DDC model
  `WK95U`). The `0xF4` values come from the ddcutil wiki's LG table and should
  apply to other recent LG models, but nothing here is verified beyond this
  panel.
- **The sidechannel never acknowledges.** `input` reports how many bus writes
  the hardware accepted, which is not the same as the monitor obeying. The only
  real confirmation is the screen changing, and `0x60` cannot be used to verify
  because it misreports.
- **`-fast` needs per-machine verification.** By default `input` sweeps every
  display mask and port, because there is no way to discover which pair
  addresses the monitor. `-fast` sends only the per-output-bit writes: ~140ms
  instead of ~1500ms. On the development machine the *combined* mask write is
  always rejected and only per-bit writes land, but confirm on yours before
  relying on it.
- **The panel's DDC engine can wedge.** After certain writes it stops answering
  and every read fails with `ERROR_GRAPHICS_I2C_ERROR_RECEIVING_DATA`. It
  recovers on a monitor power cycle. The raw I2C path keeps working throughout
  since it never touches the Windows DDC layer, so `input` and `raw` still work
  when `volume` and `brightness` do not.
- **`power off` is one-way.** Once off the panel stops answering DDC, so
  `power on` will usually fail; wake it with a signal or the joystick.
- **`go vet` reports `possible misuse of unsafe.Pointer`.** Inherent to COM
  vtable dispatch and Win32 hook callbacks, where the pointers reference
  OS-owned memory the Go garbage collector never manages. Excluded in
  `.golangci.yml` with that reasoning rather than silently.

## Layout

```
cmd/deskmux/     entry point
config/          YAML schema, defaults, loader
vcp/             protocol primitives (Code, Level, SourceAddr)
ddc/             standard DDC/CI over dxva2.dll
nvapi/           raw I2C over NvAPI_I2CWrite
winaudio/        Core Audio endpoint control
internal/app/    commands, watcher, volume keys
```

`ddc`, `nvapi`, `winaudio` and `vcp` are importable. If you are here for the
sidechannel trick, `nvapi.BuildSetVCP` and `nvapi.Client.Write` are what you
want.

## Development

```bash
make help     # list targets
make check    # fmt, vet, test -- run this before committing
make lint     # golangci-lint, fetched on demand via go run
make all      # console and windowless binaries
```

The Makefile targets POSIX make rather than assuming GNU: the `make` on a
Windows box is often BusyBox make, which silently ignores `$(shell ...)`
instead of reporting it. Nothing here depends on command substitution — the
binary derives its own version from Go's embedded VCS build info, so
`go build` alone produces an identifiable binary:

```
$ deskmux version
deskmux v0.0.0-20260902224239-ba387ed2ce87+dirty (windows/amd64, go1.27.1)
```

Stamp a release with `make build VERSION=v1.2.3`.

`make vet` passes `-unsafeptr=false`, matching the exclusion in
`.golangci.yml`, so the deliberate COM and Win32 hook conversions do not fail
the build.

Tests cover everything that runs without hardware: packet construction against
byte sequences verified on a real panel, config loading and validation, level
arithmetic, debouncing and the volume state machine.

## Credits

The `0xF4` / `0x50` mechanism is documented in the [ddcutil wiki][ddcutil], and
[meer-cha/lg-input-switch][python] implements the same approach in Python.

[ddcutil]: https://github.com/rockowitz/ddcutil/wiki/Switching-input-source-on-LG-monitors
[python]: https://github.com/meer-cha/lg-input-switch

## Licence

MIT — see [LICENSE](LICENSE).
