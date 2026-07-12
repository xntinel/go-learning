# 8. Log Sampling for High Throughput

Sampling reduces log volume while keeping important events. This lesson builds a concurrency-safe `slog.Handler` wrapper that samples Debug and Info records but always preserves Warn and Error records.

## Concepts

### Sampling at the Handler Layer

A handler wrapper sees every enabled record before output. That makes it a natural place to drop low-value logs while preserving the calling code's normal `slog` API.

### Preserve High Severity

Sampling should usually apply to Debug and Info logs, not Warn and Error logs. Dropping errors makes incidents harder to debug and hides the exact events operators need.

### Counters and Dropped Counts

A deterministic counter sampler is easy to test: log every Nth record and attach the number of records dropped since the previous emitted record. Atomic counters keep the handler safe when multiple goroutines log concurrently.

## Exercises

Edit `go.mod`:

```go
module example.com/slogsampling

go 1.26
```

### Exercise 1: Build a Sampling Handler

Create `sampling.go`:

```go
package sampling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
)

var (
	ErrNilHandler  = errors.New("handler must not be nil")
	ErrInvalidRate = errors.New("sample rate must be positive")
)

type Handler struct {
	inner        slog.Handler
	infoEvery    int64
	debugEvery   int64
	infoCount    atomic.Int64
	debugCount   atomic.Int64
	infoDropped  atomic.Int64
	debugDropped atomic.Int64
}

func New(inner slog.Handler, infoEvery, debugEvery int64) (*Handler, error) {
	if inner == nil {
		return nil, fmt.Errorf("sampling handler: %w", ErrNilHandler)
	}
	if infoEvery < 1 || debugEvery < 1 {
		return nil, fmt.Errorf("sampling handler: %w", ErrInvalidRate)
	}
	return &Handler{inner: inner, infoEvery: infoEvery, debugEvery: debugEvery}, nil
}

func (h *Handler) InfoEvery() int64 {
	return h.infoEvery
}

func (h *Handler) DebugEvery() int64 {
	return h.debugEvery
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if !h.shouldLog(r.Level) {
		return nil
	}
	clone := r.Clone()
	clone.AddAttrs(slog.Int64("dropped", h.takeDropped(r.Level)))
	return h.inner.Handle(ctx, clone)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs), infoEvery: h.infoEvery, debugEvery: h.debugEvery}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), infoEvery: h.infoEvery, debugEvery: h.debugEvery}
}

func (h *Handler) shouldLog(level slog.Level) bool {
	switch {
	case level >= slog.LevelWarn:
		return true
	case level == slog.LevelInfo:
		return h.sample(&h.infoCount, &h.infoDropped, h.infoEvery)
	case level <= slog.LevelDebug:
		return h.sample(&h.debugCount, &h.debugDropped, h.debugEvery)
	default:
		return true
	}
}

func (h *Handler) sample(count, dropped *atomic.Int64, every int64) bool {
	n := count.Add(1)
	if n%every == 0 {
		return true
	}
	dropped.Add(1)
	return false
}

func (h *Handler) takeDropped(level slog.Level) int64 {
	switch level {
	case slog.LevelInfo:
		return h.infoDropped.Swap(0)
	case slog.LevelDebug:
		return h.debugDropped.Swap(0)
	default:
		return 0
	}
}
```

### Exercise 2: Test Sampling Rules

Create `sampling_test.go`:

```go
package sampling

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func textHandler(buf *bytes.Buffer) slog.Handler {
	return slog.NewTextHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
}

func TestSamplingRates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		level     slog.Level
		every     int64
		log       func(*slog.Logger)
		wantCount int
		wantDrop  string
	}{
		{name: "info every third", level: slog.LevelInfo, every: 3, log: func(l *slog.Logger) { l.Info("sampled") }, wantCount: 2, wantDrop: "dropped=2"},
		{name: "debug every second", level: slog.LevelDebug, every: 2, log: func(l *slog.Logger) { l.Debug("sampled") }, wantCount: 3, wantDrop: "dropped=1"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			infoEvery := int64(1)
			debugEvery := int64(1)
			if tc.level == slog.LevelInfo {
				infoEvery = tc.every
			} else {
				debugEvery = tc.every
			}
			handler, err := New(textHandler(&buf), infoEvery, debugEvery)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			logger := slog.New(handler)
			for i := 0; i < 6; i++ {
				tc.log(logger)
			}

			got := buf.String()
			if strings.Count(got, "msg=sampled") != tc.wantCount {
				t.Fatalf("output = %q, want %d sampled records", got, tc.wantCount)
			}
			if !strings.Contains(got, tc.wantDrop) {
				t.Fatalf("output %q missing %q", got, tc.wantDrop)
			}
		})
	}
}

func TestWarnAndErrorAreNeverSampled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler, err := New(textHandler(&buf), 100, 100)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger := slog.New(handler)
	logger.Warn("warn event")
	logger.Error("error event")

	got := buf.String()
	if !strings.Contains(got, "warn event") || !strings.Contains(got, "error event") {
		t.Fatalf("high-severity logs were sampled: %q", got)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	_, err := New(nil, 1, 1)
	if !errors.Is(err, ErrNilHandler) {
		t.Fatalf("New(nil) error = %v, want ErrNilHandler", err)
	}
	_, err = New(textHandler(new(bytes.Buffer)), 0, 1)
	if !errors.Is(err, ErrInvalidRate) {
		t.Fatalf("New(..., 0, 1) error = %v, want ErrInvalidRate", err)
	}
}

func ExampleNew() {
	var buf bytes.Buffer
	handler, _ := New(textHandler(&buf), 2, 2)
	logger := slog.New(handler)
	logger.Info("sampled")
	logger.Info("sampled")
	fmt.Print(buf.String())
	// Output: level=INFO msg=sampled dropped=1
}
```

Your turn: add a race-safe test that logs from several goroutines and run it with `go test -race`.

### Exercise 3: Add a Demo Command

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log/slog"

	"example.com/slogsampling"
)

func main() {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	handler, err := sampling.New(inner, 2, 2)
	if err != nil {
		panic(err)
	}
	logger := slog.New(handler)
	logger.Info("sampled")
	logger.Info("sampled")
	fmt.Print(buf.String())
}
```

## Common Mistakes

Wrong: sample Warn and Error logs.

What happens: important incident evidence disappears.

Fix: always pass high-severity records through.

Wrong: store counters in ordinary integers without synchronization.

What happens: concurrent logging races under `go test -race`.

Fix: use `sync/atomic` or a mutex.

Wrong: drop records without reporting how many were skipped.

What happens: operators cannot estimate real event volume.

Fix: attach a `dropped` attribute to emitted sampled records.

## Verification

From `~/go-exercises/slog-sampling`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one concurrency test of your own before using this pattern in service code.

## Summary

- Sampling belongs well in a handler wrapper.
- Debug and Info logs can be sampled; Warn and Error logs should pass through.
- Atomic counters make the handler safe for concurrent logging.
- A `dropped` attribute makes sampling visible to operators.

## What's Next

Next, migrate global logging safely in [Replacing Global Logger Patterns](../09-replacing-global-logger-patterns/09-replacing-global-logger-patterns.md).

## Resources

- `slog.Handler` documentation: https://pkg.go.dev/log/slog#Handler
- `sync/atomic` documentation: https://pkg.go.dev/sync/atomic
- `log/slog` performance considerations: https://pkg.go.dev/log/slog#hdr-Performance_considerations
