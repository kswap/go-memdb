// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package iradix

import "sync/atomic"

// watchChannel wraps a channel so the atomic pointer below has something to
// point at, and so that an already-fired slot can be recognised by identity.
type watchChannel struct {
	ch chan struct{}
}

// closedWatchChannel is handed to anyone who asks for a channel after the slot
// has already fired. Sharing a single instance makes close idempotent and lets
// it race cleanly against a concurrent first call to channel.
var closedWatchChannel = func() *watchChannel {
	w := &watchChannel{ch: make(chan struct{})}
	close(w.ch)
	return w
}()

// watchable is the notification slot carried by every node and every leaf.
//
// Upstream allocated a chan struct{} for each of them eagerly, at eight
// construction sites, none of which were guarded by TrackMutate. A runtime
// channel object is around 96 bytes, so a node plus its leaf carried ~192 bytes
// of notification machinery — measured at roughly half the total footprint of a
// stored entry — and every workload paid it whether or not a watcher ever
// existed. Turning tracking off saved nothing, because the allocation happened
// on the way in regardless.
//
// Here the channel is created only when a reader actually asks for one, so
// writers allocate nothing at all. The zero value is ready to use and it is
// embedded by value, so making a node watchable costs no separate allocation.
type watchable struct {
	v atomic.Pointer[watchChannel]
}

// channel returns this slot's notification channel, creating it on first use.
//
// Readers of a published, nominally immutable node mutate it here, which is a
// surface the eager design did not have. It is safe because the only transition
// is nil to non-nil under compare-and-swap, and because close swaps in a
// sentinel that is already closed:
//
//   - If close lands first, the CAS below fails, the loop re-loads
//     closedWatchChannel and returns a channel that is already closed. The
//     caller observes the mutation immediately, which is correct — it happened.
//   - If the CAS lands first, close swaps that same channel out and closes it,
//     so the caller is woken normally.
//
// Either interleaving delivers the notification; neither can drop one.
func (w *watchable) channel() <-chan struct{} {
	for {
		if c := w.v.Load(); c != nil {
			return c.ch
		}
		candidate := &watchChannel{ch: make(chan struct{})}
		if w.v.CompareAndSwap(nil, candidate) {
			return candidate.ch
		}
		// Lost the race to another reader; go round and use their channel.
	}
}

// close fires the notification, if anyone ever asked for a channel. Calling it
// on a slot nobody watched is free, and calling it repeatedly is harmless.
func (w *watchable) close() {
	old := w.v.Swap(closedWatchChannel)
	if old != nil && old != closedWatchChannel {
		close(old.ch)
	}
}

// fired reports whether this slot has already been closed.
//
// Unlike channel it does not materialise anything, so asking the question about
// a slot nobody ever watched leaves it unwatched. close swaps in the shared
// sentinel, so identity against it is an exact answer.
func (w *watchable) fired() bool {
	return w.v.Load() == closedWatchChannel
}
