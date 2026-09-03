package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Autostart is a per-user Run entry, not a Windows service and not a
// scheduled task.
//
// A service is wrong outright: volumekeys installs a low-level keyboard hook
// and those are per-session, so a service in session 0 would start cleanly
// and never see a keystroke.
//
// A logon scheduled task looks right and is what this originally used, but
// schtasks refuses to create an ONLOGON trigger without elevation -- with or
// without an explicit /ru, and in a subfolder or not. Requiring an
// administrator prompt to arrange something that runs unprivileged in the
// user's own session is a poor trade.
//
// The Run key needs no privileges, starts the process inside the user's
// session where the hook and DDC both work, and is removed as easily as it is
// added. What it gives up is restart-on-failure, which matters less now that
// the daemons wait for the monitor rather than exiting when it is absent.
const _runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

const (
	_entryWatch      = "deskmux watch"
	_entryVolumeKeys = "deskmux volumekeys"
)

var _entries = []struct {
	name    string
	command string
}{
	{_entryWatch, "watch"},
	{_entryVolumeKeys, "volumekeys"},
}

// Service manages the autostart entries that run the daemons at logon.
func (a *App) Service(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: service <install|uninstall|status>")
	}

	switch args[0] {
	case "install":
		return a.serviceInstall()
	case "uninstall":
		return a.serviceUninstall()
	case "status":
		return a.serviceStatus()
	default:
		return fmt.Errorf("unknown service subcommand %q (want install, uninstall or status)", args[0])
	}
}

func (a *App) serviceInstall() error {
	exe, err := daemonExecutable()
	if err != nil {
		return err
	}

	for _, entry := range _entries {
		// Quote the executable so a path with spaces survives, and give each
		// daemon its own log since neither has a console to write to.
		command := fmt.Sprintf(`"%s" -log "%s" %s`, exe, a.logPath(entry.command), entry.command)

		line := buildCommandLine("reg", "add", _runKey,
			"/v", entry.name,
			"/t", "REG_SZ",
			"/d", command,
			"/f",
		)

		if a.opts.DryRun {
			a.printf("dry-run: %s\n", line)
			continue
		}
		if err := runCommandLine(line); err != nil {
			return fmt.Errorf("register %q: %w", entry.name, err)
		}
		a.printf("registered %q\n  %s\n", entry.name, command)
	}

	if !a.opts.DryRun {
		a.println("\nThese start at your next logon. To start them now:")
		a.printf("  %s watch\n", filepath.Base(exe))
		a.printf("  %s volumekeys\n", filepath.Base(exe))
	}
	return nil
}

func (a *App) serviceUninstall() error {
	var missing []string

	for _, entry := range _entries {
		line := buildCommandLine("reg", "delete", _runKey, "/v", entry.name, "/f")

		if a.opts.DryRun {
			a.printf("dry-run: %s\n", line)
			continue
		}
		if err := runCommandLine(line); err != nil {
			// An entry that was never registered is not worth aborting on.
			missing = append(missing, entry.name)
			a.log.Debug("autostart entry not removed", "entry", entry.name, "err", err)
			continue
		}
		a.printf("removed %q\n", entry.name)
	}

	if len(missing) > 0 {
		a.printf("not present: %s\n", strings.Join(missing, ", "))
	}
	return nil
}

func (a *App) serviceStatus() error {
	for _, entry := range _entries {
		line := buildCommandLine("reg", "query", _runKey, "/v", entry.name)

		out, err := outputOfCommandLine(line)
		if err != nil {
			a.printf("%-22s not installed\n", entry.name)
			continue
		}

		// reg query prints "    <name>    REG_SZ    <value>".
		command := "installed"
		for line := range strings.SplitSeq(out, "\n") {
			if _, value, ok := strings.Cut(line, "REG_SZ"); ok {
				command = strings.TrimSpace(value)
			}
		}
		a.printf("%-22s %s\n", entry.name, command)
	}
	return nil
}

// daemonExecutable picks the binary the autostart entries should run.
//
// The console build pops a console window on every logon and leaves one in
// the taskbar for as long as the daemon lives. The windowless build exists
// for exactly this and ships alongside, both in the release archive and via
// scoop, so prefer it when it is there.
func daemonExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}

	windowless := filepath.Join(filepath.Dir(exe), "deskmuxw.exe")
	if info, err := os.Stat(windowless); err == nil && !info.IsDir() {
		return windowless, nil
	}
	return exe, nil
}

// logPath keeps daemon logs beside the configured one, or in LocalAppData.
func (a *App) logPath(command string) string {
	if a.cfg.Log.File != "" {
		return a.cfg.Log.File
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "deskmux", command+".log")
}

// buildCommandLine quotes arguments the way the Windows command line expects.
//
// reg parses /d itself and that value contains quotes of its own. Go's exec
// would re-escape them into something reg cannot read, hence building the
// line by hand and passing it through SysProcAttr.
func buildCommandLine(name string, args ...string) string {
	var b strings.Builder
	b.WriteString(name)

	for _, arg := range args {
		b.WriteByte(' ')
		if strings.ContainsAny(arg, ` "`) {
			b.WriteString(quoteArg(arg))
			continue
		}
		b.WriteString(arg)
	}
	return b.String()
}

func quoteArg(arg string) string {
	var b strings.Builder
	b.WriteByte('"')

	for _, r := range arg {
		if r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func runCommandLine(line string) error {
	cmd := exec.Command("reg")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func outputOfCommandLine(line string) (string, error) {
	cmd := exec.Command("reg")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}

	out, err := cmd.Output()
	return string(out), err
}
