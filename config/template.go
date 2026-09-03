package config

// Template is the starter configuration written by `deskmux config init`.
//
// It is hand-written rather than produced by yaml.Marshal so that the
// comments survive -- the hardware-specific values are the part users most
// need explained, and a generated file would strip exactly that.
const Template = `# deskmux configuration
#
# Defaults below match the LG UltraGear 45GX950A, the only panel this has been
# verified against. On other monitors the input section is what you are most
# likely to need to change.

monitor: 0            # index into ` + "`deskmux probe`" + `

ddc:
  settle: 250ms       # wait before a verification read-back
  bus_delay: 40ms     # pause between raw I2C writes; below ~40ms risks
                      # wedging panels with fragile DDC engines
  source_addr: 0x51   # DDC source address for ordinary writes; only the
                      # inputs section normally departs from the standard

# Standard VCP codes. These are MCCS-standard and rarely need changing.
registers:
  brightness: 0x10
  contrast: 0x12
  volume: 0x62
  mute: 0x8D
  input_standard: 0x60   # advertised by the 45GX950A and silently ignored

# How this monitor selects its input.
#
# Most monitors use vcp 0x60 at source_addr 0x51. Recent LG panels moved input
# selection to a service sidechannel: vcp 0xF4 at source_addr 0x50 ("DDC2AB").
# If input switching does nothing, this is the section to change. The LG value
# table is documented at:
#   https://github.com/rockowitz/ddcutil/wiki/Switching-input-source-on-LG-monitors
inputs:
  vcp: 0xF4
  source_addr: 0x50
  targets:
    hdmi1: 0x90
    hdmi2: 0x91
    dp: 0xD0
    usb-c: 0xD1
  aliases:
    usbc: usb-c
    typec: usb-c
    type-c: usb-c
    tb: usb-c
    dp1: dp
    dp2: usb-c          # DP2 and USB-C share 0xD1 on this panel
    displayport: dp
    hdmi: hdmi1

pbp:
  vcp: 0xD7
  source_addr: 0x51     # PBP answers at the standard address, unlike inputs
  modes:
    off: 0x01
    "50": 0x05          # 50/50 split
    "66": 0x03          # 66/33 split, experimental

power:
  vcp: 0xD6
  source_addr: 0x51
  modes:
    on: 0x01
    off: 0x04           # one-way: the panel stops answering DDC once off

# Apply a profile when a dock appears or disappears.
#
# Find match strings for your own dock with: deskmux devices <substring>
# Set volume to -1 to leave it alone, or a level to hand over a known volume.
watch:
  match:
    - VID_1E91          # OWC Thunderbolt 3 dock, audio interface
    - VEN_OWC_TB3       # its card reader
    - SUBSYS_00191C7A   # its Intel I210 ethernet
  poll: 2s
  debounce: 2           # consecutive polls a reading must hold; docks
                        # enumerate as a burst, so 1 would race the rest
  on_dock:
    input: usb-c
    volume: -1
    power_off: false
  on_undock:
    input: dp           # push explicitly; a monitor will not abandon a live
                        # signal just because a new one appeared, so the
                        # monitor's own Auto Input Switch will not do this
    volume: -1
    power_off: false

# Volume keys drive the monitor's hardware volume instead of Windows.
#
# Only active while the default playback device name contains audio_match; on
# headphones the keys pass through to Windows untouched.
volume_keys:
  step: 1
  pin_windows: true     # hold the Windows endpoint at 100% so the monitor is
                        # the only attenuation stage
  coalesce: 20ms        # minimum interval between DDC writes while held
  audio_match: ULTRAGEAR
  audio_poll: 2s
  resync: 10s           # re-read 0x62 in case something else moved it

log:
  level: info           # debug, info, warn, error
  format: text          # text or json
  file: ""              # also append structured logs here
`
