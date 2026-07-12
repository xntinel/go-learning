# Exercise 17: Batch Processing: Size and Time Window Flush Gates

**Nivel: Intermedio** — validacion rapida (un test corto).

A logging or metrics pipeline that ships one network call per event drowns
its downstream collector in tiny requests; batching amortizes that cost,
but only if something decides when a batch is "full enough" to send. That
decision has two independent triggers — the buffer hit a size threshold, or
a time window elapsed since it was opened — and either one alone is
sufficient to flush. This module is fully self-contained: its own
`go mod init`, all code inline, its own test file.

## What you'll build

```text
batchwindow/                independent module: example.com/batch-window-flush-decision
  go.mod                    go 1.24
  flush.go                  ShouldFlush(bufSize, maxSize, opened, now, maxWindow)
  flush_test.go             table: empty, size trigger, window trigger, neither, both
```

- Files: `flush.go`, `flush_test.go`.
- Implement: `ShouldFlush(bufSize, maxSize int, opened, now time.Time, maxWindow time.Duration) bool`, guarding an empty buffer first, then the size threshold, then the elapsed-window comparison.
- Test: a table over an empty buffer, a buffer at the size threshold, a buffer under threshold but past the time window, a buffer under both, and a buffer that hits both triggers at once.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why an empty buffer needs its own guard before either trigger

`ShouldFlush` is called on a timer tick as well as after every append, so it
is routinely asked to decide about a buffer that has nothing in it yet —
the window has "elapsed" the instant the process starts, and `bufSize >=
maxSize` is trivially false only because `maxSize` is positive. Without an
explicit `bufSize == 0` guard first, a naive window check would trigger a
flush of zero events on every idle tick, which is a wasted network call for
no reason. The empty guard is not a special case bolted on; it is the same
"nothing to decide" short-circuit any guard-clause chain uses to skip
irrelevant work before evaluating the real triggers.

Create `flush.go`:

```go
// Package batchwindow decides when a batching buffer should flush, based on
// whichever of two independent triggers — buffer size or elapsed time — fires
// first.
package batchwindow

import "time"

// ShouldFlush reports whether a buffer holding bufSize items, opened at
// opened, should flush at now. It flushes if bufSize has reached maxSize, or
// if maxWindow has elapsed since opened — whichever comes first. An empty
// buffer never flushes, regardless of how much time has passed.
func ShouldFlush(bufSize, maxSize int, opened, now time.Time, maxWindow time.Duration) bool {
	if bufSize == 0 {
		return false
	}

	if bufSize >= maxSize {
		return true
	}

	if now.Sub(opened) >= maxWindow {
		return true
	}

	return false
}
```

### Tests

The table isolates each trigger — size alone, window alone, neither, both at
once — plus the empty-buffer guard, so a regression that makes either
trigger fire (or fail to fire) independently is caught without needing a
simulated buffer.

Create `flush_test.go`:

```go
package batchwindow

import (
	"testing"
	"time"
)

func TestShouldFlush(t *testing.T) {
	t.Parallel()

	opened := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const maxSize = 100
	const maxWindow = 5 * time.Second

	tests := []struct {
		name    string
		bufSize int
		now     time.Time
		want    bool
	}{
		{
			name:    "empty buffer never flushes, even long past the window",
			bufSize: 0,
			now:     opened.Add(time.Hour),
			want:    false,
		},
		{
			name:    "under both triggers, no flush",
			bufSize: 10,
			now:     opened.Add(1 * time.Second),
			want:    false,
		},
		{
			name:    "size threshold reached triggers a flush",
			bufSize: maxSize,
			now:     opened.Add(1 * time.Second),
			want:    true,
		},
		{
			name:    "time window elapsed triggers a flush even under size",
			bufSize: 1,
			now:     opened.Add(maxWindow),
			want:    true,
		},
		{
			name:    "both triggers at once still flushes",
			bufSize: maxSize,
			now:     opened.Add(maxWindow),
			want:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ShouldFlush(tc.bufSize, maxSize, opened, tc.now, maxWindow)
			if got != tc.want {
				t.Errorf("ShouldFlush(bufSize=%d, now=%v) = %v, want %v", tc.bufSize, tc.now, got, tc.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Ordering the empty-buffer guard before either trigger is what keeps an
idle timer tick from producing a flush of nothing; ordering the size check
before the window check is a performance choice, not a correctness one —
both are cheap comparisons, and the result is identical either way since
the function returns `true` the moment either is satisfied. Carry this
forward: whenever two independent conditions can each authorize the same
action, write them as two separate guards rather than one combined boolean
expression — it keeps each trigger individually testable and readable at
a glance.

## Resources

- [Go Specification: If statements](https://go.dev/ref/spec#If_statements) — the guard-clause shape this function is built from.
- [OpenTelemetry: Batch Span Processor](https://opentelemetry.io/docs/specs/otel/trace/sdk/#batching-processor) — a production batching pipeline with the same size-or-time flush trigger.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-connection-pool-eviction-lru-age.md](16-connection-pool-eviction-lru-age.md) | Next: [18-message-schema-version-negotiation.md](18-message-schema-version-negotiation.md)
