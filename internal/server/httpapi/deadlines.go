package httpapi

import (
	"io"
	"net/http"
	"time"
)

// The single http.Server (cmd/akari-server/main.go) carries a 120s ReadTimeout
// and a 60s WriteTimeout sized for the small, fast requests that make up almost
// all traffic. Those are absolute connection deadlines: they cap the whole
// request read and the whole response write regardless of progress. That is
// exactly wrong for the two large-body routes, where a multi-hundred-megabyte
// blob (the CAS holds up to 2 GiB; chunks up to 128 MiB) can legitimately take
// longer than the cap to transfer over a slow link, and the absolute deadline
// would abort an actively progressing transfer mid-stream.
//
// The fix is an idle deadline rather than an absolute one: refresh the
// connection deadline before each read or write, so a transfer that keeps making
// progress runs as long as it needs, while a stalled peer (a slow-loris upload or
// a reader that stopped consuming) still trips the deadline and frees the
// goroutine. The SSE handler uses the same per-write-deadline pattern for the
// same reason (see handleSessionEvents).
//
// idleTransferTimeout is the per-operation slack: a read or write that makes no
// progress for this long is treated as a stalled peer and aborted.
const idleTransferTimeout = 60 * time.Second

// idleDeadlineReader wraps a request body so each Read first pushes the
// connection's read deadline forward. A large upload that keeps delivering bytes
// is never cut off by the server-wide ReadTimeout, but a connection that goes
// quiet for idleTransferTimeout fails the next Read and unwinds the handler.
type idleDeadlineReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
}

// idleReadDeadline wraps body so reads extend the connection's read deadline as
// they progress. If the connection does not support deadlines (the first
// SetReadDeadline returns an error), it returns body unchanged so the server-wide
// ReadTimeout still applies as a floor rather than the wrapper silently no-oping.
func idleReadDeadline(w http.ResponseWriter, body io.Reader) io.Reader {
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Now().Add(idleTransferTimeout)) != nil {
		return body
	}
	return &idleDeadlineReader{r: body, rc: rc, idle: idleTransferTimeout}
}

func (d *idleDeadlineReader) Read(p []byte) (int, error) {
	// Best effort: a mid-stream failure to extend the deadline should not fail an
	// otherwise healthy read, so ignore the error and let the read proceed under
	// whatever deadline currently stands.
	_ = d.rc.SetReadDeadline(time.Now().Add(d.idle))
	return d.r.Read(p)
}

// idleDeadlineWriter wraps a response writer so each Write first pushes the
// connection's write deadline forward, so a large blob download to a slow client
// is not truncated by the server-wide WriteTimeout while a reader that stops
// consuming still trips the deadline.
type idleDeadlineWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

// idleWriteDeadline wraps w so writes extend the connection's write deadline as
// the body streams out. If the connection does not support deadlines it returns w
// unchanged, leaving the server-wide WriteTimeout as the floor.
func idleWriteDeadline(w http.ResponseWriter) io.Writer {
	rc := http.NewResponseController(w)
	if rc.SetWriteDeadline(time.Now().Add(idleTransferTimeout)) != nil {
		return w
	}
	return &idleDeadlineWriter{w: w, rc: rc, idle: idleTransferTimeout}
}

func (d *idleDeadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(d.idle))
	return d.w.Write(p)
}
