package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/klaidliadon/lginput/config"
)

// Watch applies a profile when the configured dock appears or disappears.
//
// The two directions are deliberately asymmetric. On connect we grab the
// panel. On disconnect the dock is on its way to another machine, so we push
// the panel to that machine's input *before* it arrives -- a monitor will not
// abandon a live signal just because a new one appears, so its own
// "auto input switch" setting cannot do this handover. Pushing early works
// because a panel will happily sit on an input that has no signal yet.
func (a *App) Watch(ctx context.Context) error {
	match := a.cfg.Watch.Match
	if len(match) == 0 {
		return errors.New("watch.match is empty: nothing to watch for")
	}

	present, id, err := devicePresent(match)
	if err != nil {
		return err
	}

	a.log.Info("watching for device",
		"match", strings.Join(match, ","),
		"poll", a.cfg.Watch.Poll.D(),
		"present_at_start", present,
		"matched", id,
	)

	ticker := time.NewTicker(a.cfg.Watch.Poll.D())
	defer ticker.Stop()

	// Device arrival enumerates as a burst, so require a reading to hold for
	// two consecutive polls before acting on it.
	const stableReads = 2

	state := present
	pending := present
	var stable int

	for {
		select {
		case <-ctx.Done():
			a.log.Info("watch stopped")
			return nil
		case <-ticker.C:
		}

		now, matched, err := devicePresent(match)
		if err != nil {
			a.log.Warn("device scan failed", "err", err)
			continue
		}

		if now != pending {
			pending, stable = now, 0
			continue
		}
		if now == state {
			continue
		}
		if stable++; stable < stableReads {
			continue
		}

		state, stable = now, 0
		if now {
			a.log.Info("dock connected", "matched", matched)
			a.applyProfile(a.cfg.Watch.OnDock, "dock")
		} else {
			a.log.Info("dock disconnected")
			a.applyProfile(a.cfg.Watch.OnUndock, "undock")
		}
	}
}

// applyProfile is best-effort: one failing step must not abort the rest, so
// failures are logged and the next step still runs.
func (a *App) applyProfile(p config.Profile, event string) {
	if p.Volume >= 0 {
		a.log.Info("profile volume", "event", event, "result", a.setVolume(uint32(p.Volume)))
	}

	if p.Input != "" {
		if err := a.Input([]string{p.Input}); err != nil {
			a.log.Error("profile input failed", "event", event, "input", p.Input, "err", err)
		} else {
			a.log.Info("profile input applied", "event", event, "input", p.Input)
		}
	}

	// Power last: the panel stops answering DDC once it is off, so nothing
	// can follow this.
	if p.PowerOff {
		if err := a.Table([]string{"off"}, a.cfg.Power, "power"); err != nil {
			a.log.Error("profile power off failed", "event", event, "err", err)
		}
	}
}
