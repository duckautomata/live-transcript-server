// Package notify provides the broadcast wakeup primitive behind the server's
// long-poll endpoints. A Notifier wakes every parked waiter at once by
// closing and replacing a signal channel; waiters loop through Wait, which
// re-subscribes before every state check so no wakeup can be lost.
package notify

import (
	"context"
	"sync"
	"time"
)

// Notifier is a broadcast signal for long polls. The zero value is not usable;
// call New.
type Notifier struct {
	mu           sync.Mutex
	signal       chan struct{}
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// New returns a ready-to-use Notifier.
func New() *Notifier {
	return &Notifier{
		signal:   make(chan struct{}),
		shutdown: make(chan struct{}),
	}
}

// Notify wakes every parked waiter by closing the current signal channel and
// replacing it with a fresh one.
func (n *Notifier) Notify() {
	n.mu.Lock()
	close(n.signal)
	n.signal = make(chan struct{})
	n.mu.Unlock()
}

// Signal returns the channel that the next Notify call will close.
func (n *Notifier) Signal() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.signal
}

// Release unblocks every parked and future Wait with the Released outcome so
// server shutdown isn't held up waiting for long polls to expire. Idempotent.
func (n *Notifier) Release() {
	n.shutdownOnce.Do(func() { close(n.shutdown) })
}

// Outcome reports why Wait returned.
type Outcome int

const (
	// Ready means check returned true.
	Ready Outcome = iota
	// Timeout means the deadline passed before check returned true.
	Timeout
	// Canceled means the caller's context was canceled.
	Canceled
	// Released means Release was called (server shutdown).
	Released
)

// Wait parks the caller until check returns true (Ready), the deadline passes
// (Timeout), ctx is canceled (Canceled), or Release is called (Released).
// check runs at least once, so a Wait whose condition already holds returns
// Ready immediately even with an expired deadline.
func (n *Notifier) Wait(ctx context.Context, deadline time.Time, check func() bool) Outcome {
	for {
		// Subscribe before checking so a Notify that lands between the check
		// and the park still closes this signal and wakes the select below.
		signal := n.Signal()

		if check() {
			return Ready
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Timeout
		}
		timer := time.NewTimer(remaining)
		select {
		case <-signal:
			timer.Stop()
			// Something changed somewhere — loop to recheck.
		case <-timer.C:
			return Timeout
		case <-ctx.Done():
			timer.Stop()
			return Canceled
		case <-n.shutdown:
			timer.Stop()
			return Released
		}
	}
}
