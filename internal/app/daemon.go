package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/klaidliadon/deskmux/config"
)

// worker is a long-running command that Daemon can host. run blocks until ctx
// is cancelled or the worker cannot continue.
type worker struct {
	name    string
	enabled func(config.Config) bool
	run     func(*App, context.Context) error
}

var _workers = []worker{
	{"watch", func(c config.Config) bool { return c.Watch.Enabled }, (*App).Watch},
	{"volumekeys", func(c config.Config) bool { return c.VolumeKeys.Enabled }, (*App).VolumeKeys},
}

// Daemon runs every enabled worker in this process until ctx is cancelled.
//
// Workers fail independently, as they did when each was its own process: one
// stopping is logged and leaves the others running.
func (a *App) Daemon(ctx context.Context) error {
	var enabled []worker
	for _, w := range a.workers {
		if w.enabled(a.cfg) {
			enabled = append(enabled, w)
		}
	}
	if len(enabled) == 0 {
		return errors.New("no workers enabled: set watch.enabled or volume_keys.enabled")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, w := range enabled {
		scoped := *a
		scoped.log = a.log.With("worker", w.name)
		wg.Go(func() {
			err := w.run(&scoped, ctx)
			if err == nil {
				return
			}
			scoped.log.Error("worker stopped", "err", err)
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", w.name, err))
			mu.Unlock()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
