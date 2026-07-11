//go:build windows

package daemon

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

type localControl struct {
	handle windows.Handle
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func eventName(inst instance) *uint16 {
	return windows.StringToUTF16Ptr("Local\\akari-daemon-" + inst.Token)
}

func startControl(_ string, inst instance, cancel context.CancelFunc) (*localControl, error) {
	handle, err := windows.CreateEvent(nil, 1, 0, eventName(inst))
	if err != nil {
		return nil, fmt.Errorf("create daemon shutdown event: %w", err)
	}
	control := &localControl{handle: handle, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(control.done)
		for {
			status, err := windows.WaitForSingleObject(handle, 100)
			if err != nil {
				return
			}
			if status == windows.WAIT_OBJECT_0 {
				cancel()
				return
			}
			select {
			case <-control.stop:
				return
			default:
			}
		}
	}()
	return control, nil
}

func (c *localControl) Close() {
	c.once.Do(func() {
		close(c.stop)
		<-c.done
		_ = windows.CloseHandle(c.handle)
	})
}

func controlReady(_ string, inst instance) bool {
	handle, err := windows.OpenEvent(windows.SYNCHRONIZE, false, eventName(inst))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(handle)
	return true
}

func requestGraceful(_ context.Context, _ string, inst instance) error {
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, eventName(inst))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetEvent(handle); err != nil {
		return fmt.Errorf("signal daemon shutdown event: %w", err)
	}
	return nil
}
