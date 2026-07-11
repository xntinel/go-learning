# 18. Building a File Watcher

Build a polling file watcher that reports changes to a single file. Production watchers often use platform event APIs or external modules, but polling is portable, deterministic, and enough to teach state snapshots, cancellation, and event delivery.

## Concepts

### Watching Means Comparing State

A watcher needs a previous snapshot and a new snapshot. For a single file, size and modification time are enough for this exercise. More advanced systems also track inode identity and rename events.

### Cancellation Is Required

A watcher is a long-running process. It must stop when the caller's context is canceled and must not leak goroutines.

### Event APIs Need Backpressure Policy

If the consumer is slow, should the watcher block, drop events, or buffer them? This lesson uses a small buffered channel and blocks when it is full, making backpressure explicit.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/pollwatch/cmd/demo
cd ~/go-exercises/pollwatch
go mod init example.com/pollwatch
```

### Exercise 1: Implement The Watcher

Create `watcher.go`:

```go
package pollwatch

import (
	"context"
	"fmt"
	"os"
	"time"
)

type Event struct {
	Path    string
	Size    int64
	ModTime time.Time
}

type Watcher struct {
	path     string
	interval time.Duration
}

func New(path string, interval time.Duration) (*Watcher, error) {
	if path == "" {
		return nil, fmt.Errorf("new watcher: %w", ErrEmptyPath)
	}
	if interval <= 0 {
		return nil, fmt.Errorf("new watcher: %w", ErrBadInterval)
	}
	return &Watcher{path: path, interval: interval}, nil
}

func (w *Watcher) Watch(ctx context.Context) (<-chan Event, <-chan error) {
	events := make(chan Event, 1)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		prev, err := snapshot(w.path)
		if err != nil {
			errs <- err
			return
		}
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := snapshot(w.path)
				if err != nil {
					errs <- err
					return
				}
				if next.Size != prev.Size || !next.ModTime.Equal(prev.ModTime) {
					prev = next
					select {
					case events <- next:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return events, errs
}

func snapshot(path string) (Event, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Event{}, fmt.Errorf("stat watched file: %w", err)
	}
	if info.IsDir() {
		return Event{}, fmt.Errorf("stat watched file: %w", ErrIsDirectory)
	}
	return Event{Path: path, Size: info.Size(), ModTime: info.ModTime()}, nil
}
```

Create `errors.go`:

```go
package pollwatch

import "errors"

var (
	ErrEmptyPath   = errors.New("path must not be empty")
	ErrBadInterval = errors.New("interval must be positive")
	ErrIsDirectory = errors.New("watched path is a directory")
)
```

### Exercise 2: Test Watching And Validation

Create `watcher_test.go`:

```go
package pollwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherReportsFileChange(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "watched.txt")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := New(path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs := w.Watch(ctx)
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Size != int64(len("changed")) {
			t.Fatalf("event = %+v", event)
		}
	case err := <-errs:
		t.Fatalf("watch error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestNewValidationErrors(t *testing.T) {
	t.Parallel()

	if _, err := New("", time.Second); !errors.Is(err, ErrEmptyPath) {
		t.Fatalf("empty path err = %v", err)
	}
	if _, err := New("x", 0); !errors.Is(err, ErrBadInterval) {
		t.Fatalf("bad interval err = %v", err)
	}
}

func TestSnapshotRejectsDirectory(t *testing.T) {
	t.Parallel()

	_, err := snapshot(t.TempDir())
	if !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("err = %v, want ErrIsDirectory", err)
	}
}

func ExampleNew() {
	w, _ := New("file.txt", time.Second)
	fmt.Println(w.interval)
	// Output: 1s
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"example.com/pollwatch"
)

func main() {
	dir, err := os.MkdirTemp("", "pollwatch-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "watched.txt")
	if err := os.WriteFile(path, []byte("start"), 0o600); err != nil {
		log.Fatal(err)
	}
	w, err := pollwatch.New(path, 10*time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errs := w.Watch(ctx)
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		log.Fatal(err)
	}
	select {
	case event := <-events:
		fmt.Println(event.Size)
	case err := <-errs:
		log.Fatal(err)
	case <-time.After(time.Second):
		log.Fatal("timed out")
	}
}
```

## Common Mistakes

### Pretending Polling Is The Same As OS Events

Wrong: claim polling sees every intermediate write.

Fix: say it observes state changes at intervals. Fast changes can collapse into one event.

### No Cancellation Path

Wrong: start a goroutine with an endless ticker and no context.

Fix: accept a context and stop the ticker when the context is canceled.

### Unbounded Event Queues

Wrong: append events forever when the consumer is slow.

Fix: choose a backpressure policy. This lesson blocks on a small buffered channel.

## Verification

Run this from `~/go-exercises/pollwatch`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test that deletes the watched file and verifies the error channel receives an error.

## Summary

- A watcher compares previous and current file state.
- Polling is portable but can miss intermediate states.
- Long-running watchers need context cancellation.
- Event channel buffering is a deliberate backpressure policy.

## What's Next

Next: [Type Parameters and Constraints](../../20-generics/01-type-parameters-and-constraints/01-type-parameters-and-constraints.md).

## Resources

- [context package](https://pkg.go.dev/context)
- [time.Ticker](https://pkg.go.dev/time#Ticker)
- [os.Stat](https://pkg.go.dev/os#Stat)
