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

// Windows scheduled tasks, not Windows services.
//
// volumekeys installs a low-level keyboard hook, and those are per-session:
// a service runs in session 0 and cannot hook the interactive desktop, so it
// would start cleanly and then never see a keystroke. watch has the same
// problem in reverse -- monitor enumeration wants a window station. A task
// triggered at logon runs as the user, inside their session, which is what
// both daemons actually need.
const (
	_taskWatch      = "deskmux watch"
	_taskVolumeKeys = "deskmux volumekeys"
)

var _tasks = []struct {
	name    string
	command string
}{
	{_taskWatch, "watch"},
	{_taskVolumeKeys, "volumekeys"},
}

// Service manages the logon tasks that run the daemons.
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
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", exe, err)
	}

	for _, task := range _tasks {
		// Quote the executable so a path with spaces survives, and let each
		// task write to its own log since a task has no console.
		action := fmt.Sprintf(`"%s" -log "%s" %s`, exe, a.taskLogPath(task.command), task.command)

		line := buildCommandLine("schtasks",
			"/create",
			"/tn", task.name,
			"/tr", action,
			"/sc", "onlogon",
			"/rl", "limited", // no elevation: DDC and the hook do not need it
			"/f", // replace an existing task of the same name
		)

		if a.opts.DryRun {
			a.printf("dry-run: %s\n", line)
			continue
		}
		if err := runCommandLine(line); err != nil {
			return fmt.Errorf("create task %q: %w", task.name, err)
		}
		a.printf("installed %q -> %s\n", task.name, action)
	}

	if !a.opts.DryRun {
		a.println("\nTasks run at logon. Start them now with:")
		a.printf("  schtasks /run /tn \"%s\"\n", _taskWatch)
		a.printf("  schtasks /run /tn \"%s\"\n", _taskVolumeKeys)
	}
	return nil
}

func (a *App) serviceUninstall() error {
	var failures []string

	for _, task := range _tasks {
		line := buildCommandLine("schtasks", "/delete", "/tn", task.name, "/f")

		if a.opts.DryRun {
			a.printf("dry-run: %s\n", line)
			continue
		}
		if err := runCommandLine(line); err != nil {
			// A task that was never installed is not a failure worth
			// aborting on; report it and carry on with the rest.
			failures = append(failures, task.name)
			a.log.Debug("task removal failed", "task", task.name, "err", err)
			continue
		}
		a.printf("removed %q\n", task.name)
	}

	if len(failures) > 0 {
		a.printf("not removed (probably never installed): %s\n", strings.Join(failures, ", "))
	}
	return nil
}

func (a *App) serviceStatus() error {
	for _, task := range _tasks {
		line := buildCommandLine("schtasks", "/query", "/tn", task.name, "/fo", "list")

		out, err := outputOfCommandLine(line)
		if err != nil {
			a.printf("%-22s not installed\n", task.name)
			continue
		}

		status := "unknown"
		for l := range strings.SplitSeq(out, "\n") {
			if name, value, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(name) == "Status" {
				status = strings.TrimSpace(value)
			}
		}
		a.printf("%-22s %s\n", task.name, status)
	}
	return nil
}

// taskLogPath keeps daemon logs beside the configured one, or in LocalAppData.
func (a *App) taskLogPath(command string) string {
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
// schtasks parses /tr itself, so its value contains quotes of its own. Go's
// exec would re-escape those and schtasks would receive something it cannot
// parse, hence building the line by hand and passing it through SysProcAttr.
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
	cmd := exec.Command("schtasks")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func outputOfCommandLine(line string) (string, error) {
	cmd := exec.Command("schtasks")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}

	out, err := cmd.Output()
	return string(out), err
}
