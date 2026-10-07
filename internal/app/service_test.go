package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A leftover per-worker shortcut races the daemon for each worker at logon,
// so status has to name it.
func TestServiceStatusReportsLegacyShortcuts(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("APPDATA", appData)
	dir, err := startupDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deskmux watch.lnk"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	a, out := newTestApp(t, Options{Monitor: -1})
	if err := a.Service([]string{"status"}); err != nil {
		t.Fatalf("service status: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, `legacy shortcut "deskmux watch" is still present`) {
		t.Errorf("status does not report the legacy shortcut:\n%s", got)
	}
	if strings.Contains(got, `"deskmux volumekeys" is still present`) {
		t.Errorf("status reports a shortcut that does not exist:\n%s", got)
	}
}

// Upgrading to `daemon` must not leave the old per-worker shortcuts behind,
// and must not touch anything else in the Startup folder.
func TestRemoveLegacyShortcuts(t *testing.T) {
	tests := []struct {
		name     string
		dryRun   bool
		wantGone bool
	}{
		{"removes the old shortcuts", false, true},
		{"dry run leaves them", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			keep := filepath.Join(dir, "Rainmeter.lnk")
			paths := make([]string, 0, 1+len(_legacyShortcuts))
			paths = append(paths, keep)
			for _, name := range _legacyShortcuts {
				paths = append(paths, filepath.Join(dir, name+".lnk"))
			}
			for _, p := range paths {
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			a, _, _, _ := newFakeApp(t, Options{Monitor: -1, DryRun: tt.dryRun})
			a.removeLegacyShortcuts(dir)

			for _, p := range paths[1:] {
				_, err := os.Stat(p)
				if gone := os.IsNotExist(err); gone != tt.wantGone {
					t.Errorf("%s gone = %v, want %v", filepath.Base(p), gone, tt.wantGone)
				}
			}
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("unrelated shortcut touched: %v", err)
			}
		})
	}
}
