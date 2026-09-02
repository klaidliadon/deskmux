package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/klaidliadon/lginput/config"
	"github.com/klaidliadon/lginput/vcp"
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
	matcher := newDeviceMatcher(a.cfg.Watch.Match)
	if matcher.empty() {
		return errors.New("watch.match is empty: nothing to watch for")
	}

	present, id, err := matcher.find()
	if err != nil {
		return err
	}

	a.log.Info("watching for device",
		"match", strings.Join(a.cfg.Watch.Match, ","),
		"poll", a.cfg.Watch.Poll.D(),
		"present_at_start", present,
		"matched", id,
	)

	ticker := time.NewTicker(a.cfg.Watch.Poll.D())
	defer ticker.Stop()

	debounce := newDebouncer(present, _stableReads)

	for {
		select {
		case <-ctx.Done():
			a.log.Info("watch stopped")
			return nil
		case <-ticker.C:
		}

		now, matched, err := matcher.find()
		if err != nil {
			a.log.Warn("device scan failed", "err", err)
			continue
		}
		if !debounce.observe(now) {
			continue
		}

		if now {
			a.log.Info("dock connected", "matched", matched)
			a.applyProfile(a.cfg.Watch.OnDock, "dock")
		} else {
			a.log.Info("dock disconnected")
			a.applyProfile(a.cfg.Watch.OnUndock, "undock")
		}
	}
}

// Device arrival enumerates as a burst as each function of a dock appears, so
// a reading has to hold before it is believed.
const _stableReads = 2

// debouncer reports a state change only once a new reading has held for a
// number of consecutive observations.
type debouncer struct {
	state   bool
	pending bool
	stable  int
	needed  int
}

func newDebouncer(initial bool, needed int) *debouncer {
	return &debouncer{state: initial, pending: initial, needed: needed}
}

// observe records a reading and reports whether it constitutes a change.
//
// The first sighting of a new value counts towards the total, so needed=2
// means two observations, not three. Starting the count at zero here was an
// off-by-one that made the watcher react a whole poll later than configured.
func (d *debouncer) observe(reading bool) (changed bool) {
	switch {
	case reading != d.pending:
		d.pending, d.stable = reading, 1
	case reading != d.state:
		d.stable++
	}

	if reading == d.state || d.stable < d.needed {
		return false
	}

	d.state, d.stable = reading, 0
	return true
}

// applyProfile is best-effort: one failing step must not abort the rest, so
// failures are logged and the next step still runs.
func (a *App) applyProfile(p config.Profile, event string) {
	if p.Volume >= 0 {
		a.log.Info("profile volume", "event", event, "result", a.setVolume(vcp.Level(p.Volume)))
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
