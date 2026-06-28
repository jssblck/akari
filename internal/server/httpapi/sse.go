package httpapi

import "sync"

// sseHub fans session-update notifications out to any connected browsers
// watching a session. It carries no payload: a notification just tells a watcher
// to re-fetch the session body fragment, so the rendering path stays in one
// place (the body handler) and the SSE channel stays trivially small.
type sseHub struct {
	mu   sync.Mutex
	subs map[int64]map[chan struct{}]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{subs: make(map[int64]map[chan struct{}]struct{})}
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
