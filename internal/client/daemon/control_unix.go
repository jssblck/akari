//go:build !windows

package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

type localControl struct {
	listener *net.UnixListener
	path     string
	done     chan struct{}
	once     sync.Once
}

func controlPath(pidfile string) string { return pidfile + ".sock" }

func startControl(pidfile string, inst instance, cancel context.CancelFunc) (*localControl, error) {
	path := controlPath(pidfile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale daemon control socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on daemon control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.Remove(path)
		return nil, fmt.Errorf("secure daemon control socket: %w", err)
	}
	control := &localControl{listener: listener, path: path, done: make(chan struct{})}
	go control.serve(inst.Token, cancel)
	return control, nil
}

func (c *localControl) serve(token string, cancel context.CancelFunc) {
	defer close(c.done)
	for {
		conn, err := c.listener.AcceptUnix()
		if err != nil {
			return
		}
		valid := false
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && strings.TrimSpace(line) == token {
			valid = true
			_, _ = conn.Write([]byte("ok\n"))
		}
		_ = conn.Close()
		if valid {
			cancel()
			return
		}
	}
}

func (c *localControl) Close() {
	c.once.Do(func() {
		_ = c.listener.Close()
		<-c.done
		_ = os.Remove(c.path)
	})
}

func controlReady(pidfile string, _ instance) bool {
	conn, err := net.DialTimeout("unix", controlPath(pidfile), 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func requestGraceful(ctx context.Context, pidfile string, inst instance) error {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", controlPath(pidfile))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintln(conn, inst.Token); err != nil {
		return err
	}
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(response) != "ok" {
		return fmt.Errorf("daemon rejected shutdown request")
	}
	return nil
}
