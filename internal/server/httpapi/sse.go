package httpapi

import "sync"

// sseHub fans session-update notifications out to any connected browsers
// watching a session. The per-session channel carries no payload: a notification
// just tells a watcher to re-fetch the session body fragment, so the rendering path
// stays in one place (the body handler) and the channel stays trivially small.
//
// It also carries a single fleet-wide reparse-progress channel set. Unlike the
// session signal, that one carries a payload (the status JSON), so a watching page
// updates its progress bar straight from the event without a round trip.
type sseHub struct {
	mu      sync.Mutex
	subs    map[int64]map[chan struct{}]struct{}
	reparse map[chan string]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{
		subs:    make(map[int64]map[chan struct{}]struct{}),
		reparse: make(map[chan string]struct{}),
	}
}

// subscribeReparse registers a watcher for reparse progress and returns its
// channel. The channel is buffered by one and the publisher keeps only the latest
// value in it, so a slow reader always sees the most recent progress (and, at the
// end, the terminal status) even if it missed intermediate frames.
func (h *sseHub) subscribeReparse() chan string {
	ch := make(chan string, 1)
	h.mu.Lock()
	h.reparse[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribeReparse(ch chan string) {
	h.mu.Lock()
	delete(h.reparse, ch)
	h.mu.Unlock()
}

// publishReparse delivers the latest reparse status to every watcher. It drains a
// stale value before sending so the buffer holds the newest payload rather than
// dropping the new one when a reader has not caught up; progress frames are
// coalescible, and the final status is the one that matters.
func (h *sseHub) publishReparse(payload string) {
	h.mu.Lock()
	for ch := range h.reparse {
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- payload:
		default:
		}
	}
	h.mu.Unlock()
}

// subscribe registers a watcher for a session and returns its notify channel.
// The channel is buffered by one so a publish never blocks on a slow reader; a
// coalesced "you have updates" signal is all a watcher needs.
func (h *sseHub) subscribe(sessionID int64) chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[chan struct{}]struct{})
	}
	h.subs[sessionID][ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(sessionID int64, ch chan struct{}) {
	h.mu.Lock()
	if m := h.subs[sessionID]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.subs, sessionID)
		}
	}
	h.mu.Unlock()
}

// publish wakes every watcher of a session. A non-blocking send keeps a watcher
// that has not yet drained its previous signal from stalling the publisher.
func (h *sseHub) publish(sessionID int64) {
	h.mu.Lock()
	for ch := range h.subs[sessionID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}
