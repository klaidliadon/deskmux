// Command lginput controls DDC/CI monitors on Windows, including input
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
	"syscall"

	"github.com/klaidliadon/lginput/config"
	"github.com/klaidliadon/lginput/internal/app"
)

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
	fs := flag.NewFlagSet("lginput", flag.ContinueOnError)
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
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	closeLog := func() {}
	var sink io.Writer = os.Stderr

	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", cfg.File, err)
		}
		closeLog = func() { f.Close() }
		sink = io.MultiWriter(os.Stderr, f)
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler = slog.NewTextHandler(sink, opts)
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(sink, opts)
	}
	return slog.New(handler), closeLog, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `lginput - DDC/CI monitor control for Windows

usage: lginput [flags] <command> [args]

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
