package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestRunReturnsWhenWatcherChannelsClose(t *testing.T) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsw.Close(); err != nil {
		t.Fatal(err)
	}

	w := New(nil, nil, Options{})
	if err := w.run(context.Background(), fsw); !errors.Is(err, errWatcherClosed) {
		t.Fatalf("run after watcher shutdown = %v, want %v", err, errWatcherClosed)
	}
}
