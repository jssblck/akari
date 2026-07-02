package httpapi

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// serveSSE is the shared spine of the SSE endpoints: it sets the stream headers,
// opens the stream with a comment, then writes one frame per value received on ch
// (rendered by frame) and a keepalive comment every 25s, until the client goes
// away or a write fails. Each write gets a bounded deadline rather than clearing
// the deadline for the whole stream: a client that stops reading would otherwise
// block the write forever, so the caller's deferred unsubscribe never runs and
// the subscription leaks. A short deadline turns a stalled client into a write
// error, ending the loop. onOpen, when non-nil, writes any initial frames right
// after the stream opens and reports whether to continue (the reparse stream
// paints the current status so a page connecting mid-run does not wait for the
// next frame).
func serveSSE[T any](w http.ResponseWriter, r *http.Request, ch <-chan T, frame func(T) string, onOpen func(write func(string) bool) bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	write := func(payload string) bool {
		if rc.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprint(w, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	// An initial comment opens the stream so the browser's EventSource fires open.
	if !write(": connected\n\n") {
		return
	}
	if onOpen != nil && !onOpen(write) {
		return
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-ch:
			if !write(frame(v)) {
				return
			}
		case <-keepalive.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}

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
