// Command deskmux controls DDC/CI monitors on Windows, including input
// switching on LG panels that advertise the standard register and then
// silently ignore it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/klaidliadon/deskmux/config"
	"github.com/klaidliadon/deskmux/internal/app"
)

// version may be stamped at build time with
// -ldflags "-X main.version=v1.2.3", which is what a tagged release should
// do. When it is not, buildVersion falls back to the VCS revision Go embeds
// automatically, so a plain `go build` still produces an identifiable binary.
var version string

// buildVersion reports the most specific version available.
//
// Deriving this in Go rather than in the Makefile is deliberate: `make` is not
// always GNU make -- BusyBox make, for one, silently ignores $(shell ...) and
// would stamp an empty string without complaining.
func buildVersion() string {
	if version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	// Set when installed with `go install module@version`.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}

	var revision, suffix string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				suffix = "-dirty"
			}
		}
	}
	if revision == "" {
		return "devel"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision + suffix
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, app.ErrUsage) || errors.Is(err, flag.ErrHelp) {
			usage()
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("deskmux", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage() handles presentation
	fs.Usage = usage

	var (
		configPath = fs.String("config", "", "configuration `file` (default: search standard locations)")
		monitor    = fs.Int("m", -1, "monitor index, overriding the config")
		dryRun     = fs.Bool("n", false, "dry run: show what would be sent, send nothing")
		verbose    = fs.Bool("v", false, "verbose output and debug logging")
		fast       = fs.Bool("fast", false, "input: skip the port sweep (much faster; verify it works first)")
		logFile    = fs.String("log", "", "append structured logs to this `file`, overriding the config")
		logLevel   = fs.String("log-level", "", "debug, info, warn or error, overriding the config")
	)

	if err := fs.Parse(argv); err != nil {
		return err
	}

	args := fs.Args()
	if len(args) == 0 {
		return app.ErrUsage
	}

	// Answered before loading configuration: `version` must work even when
	// the config file is missing or malformed.
	if args[0] == "version" {
		fmt.Printf("deskmux %s (%s/%s, %s)\n",
			buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}

	cfg, usedPath, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if *logFile != "" {
		cfg.Log.File = *logFile
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *verbose {
		cfg.Log.Level = "debug"
	}

	logger, closeLog, err := newLogger(cfg.Log)
	if err != nil {
		return err
	}
	defer closeLog()

	if usedPath != "" {
		logger.Debug("configuration loaded", "path", usedPath)
	} else {
		logger.Debug("no configuration file found, using defaults",
			"searched", config.SearchPaths())
	}

	// Ctrl+C and SIGTERM cancel the context, which the daemons watch.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := app.New(cfg, logger, os.Stdout, app.Options{
		DryRun:  *dryRun,
		Verbose: *verbose,
		Fast:    *fast,
		Monitor: *monitor,
	})
	return a.Run(ctx, args[0], args[1:])
}

func newLogger(cfg config.Log) (*slog.Logger, func(), error) {
	// slog.Level implements encoding.TextUnmarshaler, which handles case and
	// offsets such as "warn+2" for free, and reports a bad value instead of
	// silently falling back to info.
	level := slog.LevelInfo
	if cfg.Level != "" {
		if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
			return nil, nil, fmt.Errorf("log level %q: %w", cfg.Level, err)
		}
	}

	closeLog := func() {}
	var sink io.Writer = os.Stderr

	if cfg.File != "" {
		// Create the directory: a scheduled task pointed at a log under
		// LocalAppData would otherwise fail to start on a fresh machine,
		// with no console to report why.
		if dir := filepath.Dir(cfg.File); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, fmt.Errorf("create log directory %s: %w", dir, err)
			}
		}

		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", cfg.File, err)
		}
		closeLog = func() { _ = f.Close() }

		// Only tee to stderr when there is a stderr worth writing to.
		//
		// The windowless build has no console, so os.Stderr is not a usable
		// handle. io.MultiWriter returns on the first write error, so
		// including a broken stderr silently discarded every log line -- in
		// exactly the configuration the daemons are meant to run in.
		sink = f
		if hasUsableStderr() {
			sink = io.MultiWriter(f, os.Stderr)
		}
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler = slog.NewTextHandler(sink, opts)
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(sink, opts)
	}
	return slog.New(handler), closeLog, nil
}

// hasUsableStderr reports whether stderr can actually be written to. It
// cannot in a GUI-subsystem process launched without a console.
func hasUsableStderr() bool {
	if os.Stderr == nil {
		return false
	}
	_, err := os.Stderr.Stat()
	return err == nil
}

func usage() {
	fmt.Fprint(os.Stderr, `deskmux - DDC/CI monitor control for Windows

usage: deskmux [flags] <command> [args]

daemons:
  watch                   apply a profile when the configured dock appears
  volumekeys              volume keys drive the monitor's own volume

control:
  input <target>          switch the input source
  volume     [v|+v|-v]    monitor volume
  brightness [v|+v|-v]    monitor brightness
  mute | unmute           monitor audio mute
  pbp <mode>              picture-by-picture
  power <on|off>          monitor power
  wake                    wake this machine's own display output

diagnostics:
  probe                   read every configured register, plus capabilities
  get <vcp>               read one VCP code            (get 0x10)
  set <vcp> <value>       write one VCP code, verified (set 0x62 30)
  raw <vcp> <value>       hand-built packet over raw I2C, bypassing the
                          Windows DDC layer entirely
  devices [substr]        list present device IDs, to find watch match strings

configuration:
  config init [path]      write a documented starter config
  config show             print the starter config to stdout
  config path             show where configuration is searched for
  version                 print the version and toolchain

service (per-user autostart entries, run at logon as you):
  service install         register watch and volumekeys to start at logon
  service uninstall       remove them
  service status          report whether they are installed

flags (must precede the command):
  -config file    configuration file
  -m N            monitor index, overriding the config
  -n              dry run
  -v              verbose output and debug logging
  -fast           input: skip the port sweep
  -log file       append structured logs to a file
  -log-level L    debug, info, warn or error

DDC/CI writes are fire-and-forget: success means the bytes were sent, not
that the monitor obeyed. Commands that can verify do so by reading back.
Input switching cannot be verified, so judge it by looking at the screen.
`)
}
