package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klaidliadon/deskmux/config"
)

// fakeWorker records that it ran and blocks until ctx is cancelled, unless
// told to fail straight away.
func fakeWorker(name string, on bool, fail error, ran *sync.Map) worker {
	return worker{
		name:    name,
		enabled: func(config.Config) bool { return on },
		run: func(a *App, ctx context.Context) error {
			ran.Store(name, true)
			a.log.Info("started")
			if fail != nil {
				return fail
			}
			<-ctx.Done()
			return nil
		},
	}
}

func newDaemonApp(t *testing.T, workers ...worker) (*App, *bytes.Buffer) {
	t.Helper()
	a, _, _, _ := newFakeApp(t, liveOpts())
	var logs syncBuffer
	a.log = slog.New(slog.NewTextHandler(&logs, nil))
	a.workers = workers
	return a, &logs.buf
}

// syncBuffer serialises writes from concurrent workers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func TestDaemonRunsOnlyEnabledWorkers(t *testing.T) {
	var ran sync.Map
	a, logs := newDaemonApp(t,
		fakeWorker("on", true, nil, &ran),
		fakeWorker("off", false, nil, &ran),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := a.Daemon(ctx); err != nil {
		t.Fatalf("Daemon: %v", err)
	}
	if _, ok := ran.Load("on"); !ok {
		t.Error("enabled worker did not run")
	}
	if _, ok := ran.Load("off"); ok {
		t.Error("disabled worker ran")
	}
	if !strings.Contains(logs.String(), "worker=on") {
		t.Errorf("log lines lack the worker name:\n%s", logs)
	}
}

func TestDaemonRefusesToRunNothing(t *testing.T) {
	var ran sync.Map
	a, _ := newDaemonApp(t, fakeWorker("off", false, nil, &ran))

	if err := a.Daemon(t.Context()); err == nil {
		t.Fatal("expected an error with no worker enabled")
	}
}

// One worker failing must not take the other down: they used to be separate
// processes, and folding them together should not couple their lifetimes.
func TestDaemonKeepsRunningWhenOneWorkerFails(t *testing.T) {
	var ran sync.Map
	boom := errors.New("boom")
	a, logs := newDaemonApp(t,
		fakeWorker("broken", true, boom, &ran),
		fakeWorker("healthy", true, nil, &ran),
	)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Daemon(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Daemon returned while a worker was still healthy: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	err := <-done
	if !errors.Is(err, boom) {
		t.Errorf("Daemon error = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "broken:") {
		t.Errorf("error %q does not name the failed worker", err)
	}
	if !strings.Contains(logs.String(), `msg="worker stopped" worker=broken`) {
		t.Errorf("failure not logged against the worker:\n%s", logs)
	}
}
