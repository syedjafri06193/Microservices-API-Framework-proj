package lifecycle

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// WaitForSignal blocks until SIGTERM or SIGINT arrives, or the context is
// cancelled, and reports which.
//
// A second signal is deliberately *not* handled here. Some services want a
// second Ctrl-C to abort the drain and exit immediately; others must never
// skip the drain because they hold a lease that has to be released. That is
// a per-service decision, so the framework surfaces the channel and lets
// the caller choose rather than picking for everyone.
func WaitForSignal(ctx context.Context) os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(ch)

	select {
	case sig := <-ch:
		return sig
	case <-ctx.Done():
		return nil
	}
}

// NotifyContext returns a context cancelled on SIGTERM or SIGINT, and a
// stop function. The stop function must be called to release the signal
// handler, or the process keeps a handler registered for a context nobody
// is watching.
func NotifyContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
}
