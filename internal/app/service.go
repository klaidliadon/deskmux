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

// Autostart is a shortcut in the user's Startup folder -- not a Windows
// service, not a scheduled task, and no longer a Run key entry.
//
// A service is wrong outright: volumekeys installs a low-level keyboard hook
// and those are per-session, so a service in session 0 would start cleanly
// and never see a keystroke.
//
// A logon scheduled task looks right, but schtasks refuses to create an
// ONLOGON trigger without elevation -- with or without an explicit /ru, and in
// a subfolder or not. Requiring an administrator prompt to arrange something
// that runs unprivileged in the user's own session is a poor trade.
//
// The HKCU Run key was used up to v0.3.0 and looked correct from every angle:
// well-formed values, enabled in Task Manager, and the exact command lines ran
// fine by hand. Explorer skipped them at every logon anyway, while running the
// key's other entries on either side of them, and logged no reason. A
// controlled logon settled it: four Run entries varying the value name (with
// and without a space) and the path (scoop's junction and the real directory)
// were all skipped, while a Startup-folder shortcut to the junction ran. What
// the skipped entries share and the executed ones lack is an unsigned binary,
// but that is inference; the measured fact is that the Startup folder works
// where the Run key does not.
//
// The Startup folder keeps everything that made the Run key attractive: no
// privileges, the process starts in the user's session where the hook and DDC
// both work, and removing it is deleting a file.
const _runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

var _entries = []struct {
	name    string // shortcut file name, without .lnk
	command string
}{
	{"deskmux watch", "watch"},
	{"deskmux volumekeys", "volumekeys"},
}

// _legacyRunValues are the Run key registrations used up to v0.3.0. Install
// and uninstall both remove them: they never ran, but left behind they would
// make a broken registration look plausible in Task Manager.
var _legacyRunValues = []string{"deskmux watch", "deskmux volumekeys"}

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
	dir, err := startupDir()
	if err != nil {
		return err
	}

	for _, entry := range _entries {
		lnk := filepath.Join(dir, entry.name+".lnk")
		// Give each daemon its own log, since neither has a console.
		args := daemonArgs(a.logPath(entry.command), entry.command)

		if a.opts.DryRun {
			a.printf("dry-run: shortcut %s\n  -> \"%s\" %s\n", lnk, exe, args)
			continue
		}
		if err := writeShortcut(lnk, exe, args); err != nil {
			return fmt.Errorf("create %s: %w", lnk, err)
		}
		a.printf("installed %s\n  -> \"%s\" %s\n", lnk, exe, args)
	}

	a.removeLegacyRunValues()

	if !a.opts.DryRun {
		a.println("\nThese start at your next logon. To start them now:")
		a.printf("  %s watch\n", filepath.Base(exe))
		a.printf("  %s volumekeys\n", filepath.Base(exe))
	}
	return nil
}

func (a *App) serviceUninstall() error {
	dir, err := startupDir()
	if err != nil {
		return err
	}

	var missing []string
	for _, entry := range _entries {
		lnk := filepath.Join(dir, entry.name+".lnk")

		if a.opts.DryRun {
			a.printf("dry-run: remove %s\n", lnk)
			continue
		}
		if err := os.Remove(lnk); err != nil {
			// An entry that was never installed is not worth aborting on.
			missing = append(missing, entry.name)
			a.log.Debug("autostart shortcut not removed", "path", lnk, "err", err)
			continue
		}
		a.printf("removed %s\n", lnk)
	}

	if len(missing) > 0 {
		a.printf("not present: %s\n", strings.Join(missing, ", "))
	}

	a.removeLegacyRunValues()
	return nil
}

// removeLegacyRunValues deletes the pre-v0.3.1 Run key registrations. Absence
// is the normal case and is not reported.
func (a *App) removeLegacyRunValues() {
	for _, name := range _legacyRunValues {
		line := buildCommandLine("reg", "delete", _runKey, "/v", name, "/f")

		if a.opts.DryRun {
			a.printf("dry-run: %s\n", line)
			continue
		}
		if err := runCommandLine(line); err == nil {
			a.printf("removed legacy Run key entry %q\n", name)
		}
	}
}

func (a *App) serviceStatus() error {
	dir, err := startupDir()
	if err != nil {
		return err
	}

	for _, entry := range _entries {
		lnk := filepath.Join(dir, entry.name+".lnk")

		target, err := readShortcut(lnk)
		if err != nil {
			a.printf("%-20s not installed\n", entry.name)
			continue
		}
		a.printf("%-20s %s\n", entry.name, target)
	}

	// Worth saying out loud: a leftover Run value looks installed in Task
	// Manager and never starts.
	for _, name := range _legacyRunValues {
		line := buildCommandLine("reg", "query", _runKey, "/v", name)
		if _, err := outputOfCommandLine(line); err == nil {
			a.printf("\nlegacy Run key entry %q is still present and will not start;\n"+
				"`service install` or `service uninstall` removes it\n", name)
		}
	}
	return nil
}

// startupDir is the per-user Startup folder. It lives under %APPDATA%, so
// folder redirection of AppData is honoured.
func startupDir() (string, error) {
	appData, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate AppData: %w", err)
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup"), nil
}

// daemonArgs is the argument string a shortcut passes to the daemon.
func daemonArgs(logPath, command string) string {
	return fmt.Sprintf(`-log "%s" %s`, logPath, command)
}

// Shortcuts are written and read through the WScript.Shell COM object via
// PowerShell. The .lnk format is documented but sprawling, and a hand-rolled
// writer would be the largest piece of code in the package for something done
// once at install time. Values travel in environment variables rather than on
// the command line, so paths with quotes or spaces need no escaping at all.
const (
	_writeShortcutScript = `$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:DESKMUX_LNK)
$s.TargetPath = $env:DESKMUX_TARGET
$s.Arguments = $env:DESKMUX_ARGS
$s.WorkingDirectory = Split-Path -Parent $env:DESKMUX_TARGET
$s.Description = 'deskmux daemon'
$s.Save()`

	_readShortcutScript = `if (-not (Test-Path -LiteralPath $env:DESKMUX_LNK)) { exit 2 }
$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:DESKMUX_LNK)
'"' + $s.TargetPath + '" ' + $s.Arguments`
)

func writeShortcut(lnk, target, args string) error {
	_, err := runPowerShell(_writeShortcutScript,
		"DESKMUX_LNK="+lnk, "DESKMUX_TARGET="+target, "DESKMUX_ARGS="+args)
	return err
}

func readShortcut(lnk string) (string, error) {
	return runPowerShell(_readShortcutScript, "DESKMUX_LNK="+lnk)
}

func runPowerShell(script string, env ...string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// daemonExecutable picks the binary the autostart entries should run.
//
// The console build pops a console window on every logon and leaves one in
// the taskbar for as long as the daemon lives. The windowless build exists
// for exactly this and ships alongside, both in the release archive and via
// scoop, so prefer it when it is there.
//
// Scoop's `current` junction is kept rather than resolved: the logon test
// above showed Explorer follows it from a shortcut, and it is what keeps the
// shortcut pointing at the new version after `scoop update`.
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
// reg parses its own arguments and some values contain quotes. Go's exec
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
