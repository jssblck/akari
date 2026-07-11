package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
)

var errPasswordWorkUnavailable = errors.New("password work unavailable")

type passwordOperations struct {
	hash   func(string) (string, error)
	verify func(string, string) (bool, error)
}

// passwordWork owns the process-wide admission boundary for request-triggered
// Argon2 work. The admitted channel bounds active workers plus queued callers,
// while slots bounds the memory-heavy work itself.
type passwordWork struct {
	admitted   chan struct{}
	slots      chan struct{}
	wait       time.Duration
	operations passwordOperations
	dummyHash  string
}

var (
	dummyPasswordOnce sync.Once
	dummyPasswordHash string
	dummyPasswordErr  error
)

func newPasswordWork(cfg config.Server) *passwordWork {
	workers := cfg.PasswordWorkers
	if workers <= 0 {
		workers = config.DefaultPasswordWorkers
	}
	queueDepth := cfg.PasswordQueueDepth
	if queueDepth <= 0 {
		queueDepth = config.DefaultPasswordQueueDepth
	}
	wait := cfg.PasswordQueueTimeout
	if wait <= 0 {
		wait = config.DefaultPasswordQueueTimeout
	}
	w := newPasswordWorkWithOperations(workers, queueDepth, wait, passwordOperations{
		hash:   auth.HashPassword,
		verify: auth.VerifyPassword,
	})

	// The dummy hash is expensive to create and identical across Server instances,
	// so build it once through the same admission boundary used by registrations.
	// A missing OS entropy source makes password hashing unusable and is therefore
	// a startup invariant failure rather than a recoverable request error.
	dummyPasswordOnce.Do(func() {
		dummyPasswordHash, dummyPasswordErr = w.Hash(context.Background(), "akari timing-normalization password")
	})
	if dummyPasswordErr != nil {
		panic("initialize dummy password hash: " + dummyPasswordErr.Error())
	}
	w.dummyHash = dummyPasswordHash
	return w
}

func newPasswordWorkWithOperations(workers, queueDepth int, wait time.Duration, operations passwordOperations) *passwordWork {
	if workers <= 0 {
		panic("password workers must be positive")
	}
	if queueDepth < 0 {
		panic("password queue depth must not be negative")
	}
	if wait <= 0 {
		panic("password queue timeout must be positive")
	}
	return &passwordWork{
		admitted:   make(chan struct{}, workers+queueDepth),
		slots:      make(chan struct{}, workers),
		wait:       wait,
		operations: operations,
	}
}

func (w *passwordWork) Hash(ctx context.Context, password string) (string, error) {
	var hash string
	err := w.do(ctx, func() error {
		var err error
		hash, err = w.operations.hash(password)
		return err
	})
	return hash, err
}

func (w *passwordWork) Verify(ctx context.Context, password, encoded string) (bool, error) {
	var ok bool
	err := w.do(ctx, func() error {
		var err error
		ok, err = w.operations.verify(password, encoded)
		return err
	})
	return ok, err
}

func (w *passwordWork) do(ctx context.Context, run func() error) error {
	select {
	case w.admitted <- struct{}{}:
		defer func() { <-w.admitted }()
	default:
		return errPasswordWorkUnavailable
	}

	timer := time.NewTimer(w.wait)
	defer timer.Stop()
	select {
	case w.slots <- struct{}{}:
		defer func() { <-w.slots }()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errPasswordWorkUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}
