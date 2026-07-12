# Exercise 9: Rate-limited error logging to survive a hot failing dependency

When one downstream dependency starts failing, it fails on the hot path — millions
of times a minute — and if every failure logs a line you get three disasters at
once: the real signal drowns, the log pipeline saturates, and the bill explodes.
This module builds a sampling logger that emits the first N occurrences of an error
signature, then throttles to one line per interval carrying a suppressed-count,
while incrementing the error metric on *every* occurrence — so the metric stays
exact and the logs stay affordable.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
logsample/                   independent module: example.com/logsample
  go.mod                     go 1.25
  sampler.go                 Sampler: first-N then throttle per signature; exact metric; injected clock
  cmd/
    demo/
      main.go                runnable demo: a burst of one signature, sampled
  sampler_test.go            10000 errors -> metric 10000, few log lines; suppressed count; -race
```

- Files: `sampler.go`, `cmd/demo/main.go`, `sampler_test.go`.
- Implement: a `Sampler` with per-signature state in a `sync.Map`; `Record(ctx, sig, err)` increments an exact `atomic.Int64` total, logs the first N per signature, then at most one line per interval carrying the suppressed count; clock injected as `now func() time.Time`.
- Test: fire 10000 identical errors with a fake clock; assert the metric is exactly 10000 but the buffer holds only the sampled lines; advance the clock and assert a throttled line reports a plausible suppressed count; run under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### The metric is exact; the log is sampled

The core insight is that logs and metrics have opposite cost profiles, so they
deserve opposite policies under a storm. A metric increment is a single atomic add
— effectively free, and its whole value is being an exact aggregate — so it fires
on every occurrence. A log line is expensive (serialization, I/O, storage, and in
a hosted backend, money) and its value is detail you only need a few samples of —
so it is rate-limited. `Record` therefore always does `total.Add(1)` first, then
decides whether to *log*.

The sampling policy is per *signature* (a stable key for "the same error," e.g.
`"db.query timeout"`), because a storm is one signature repeating, and you want the
first few instances of *each* distinct failure, not the first few globally. For a
signature the policy is: log the first N occurrences in full (you want to see it
immediately when it starts), then throttle to at most one line per `interval`, and
that throttled line carries how many were suppressed since the last emit — so the
operator reading it knows the true volume even though most lines were dropped.

State per signature lives in a `sync.Map` (a map keyed by signature, each value a
small struct guarded by its own mutex). `sync.Map` fits because the key set is
read-mostly and grows slowly (one entry per distinct signature), and per-entry
locking keeps different signatures from contending. The clock is injected as a
`now func() time.Time` so the test can fire ten thousand errors and advance time
by hand — the same clock-injection pattern you would otherwise avoid with
`synctest`, used here because the throttle interval is the unit under test.

Create `sampler.go`:

```go
package logsample

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// sigState is the per-signature throttle state, guarded by its own mutex.
type sigState struct {
	mu         sync.Mutex
	count      uint64    // total occurrences of this signature
	suppressed uint64    // dropped since the last emitted line
	lastEmit   time.Time // when a line was last emitted for this signature
}

// Sampler rate-limits error logging per signature while keeping an exact total
// metric. It logs the first N occurrences of a signature, then at most one line
// per interval carrying the suppressed count.
type Sampler struct {
	logger   *slog.Logger
	now      func() time.Time
	first    uint64
	interval time.Duration
	total    atomic.Int64
	states   sync.Map // signature -> *sigState
}

// NewSampler builds a Sampler. first is how many occurrences of each signature to
// log in full; interval is the minimum gap between throttled lines; now injects
// the clock (pass time.Now in production).
func NewSampler(logger *slog.Logger, first uint64, interval time.Duration, now func() time.Time) *Sampler {
	return &Sampler{logger: logger, now: now, first: first, interval: interval}
}

// Total returns the exact number of errors recorded, sampled or not (the metric).
func (s *Sampler) Total() int64 { return s.total.Load() }

// Record counts the error (always) and logs it (sometimes) under the given
// signature. The signature groups "the same" error; a storm is one signature.
func (s *Sampler) Record(ctx context.Context, sig string, err error) {
	s.total.Add(1) // metric: exact, every occurrence

	v, _ := s.states.LoadOrStore(sig, &sigState{})
	st := v.(*sigState)

	st.mu.Lock()
	defer st.mu.Unlock()
	st.count++

	// First N occurrences: always log in full.
	if st.count <= s.first {
		s.emit(ctx, sig, err, st.count, 0)
		st.lastEmit = s.now()
		return
	}

	// Beyond N: at most one line per interval, reporting how many were dropped.
	now := s.now()
	if now.Sub(st.lastEmit) >= s.interval {
		suppressed := st.suppressed
		st.suppressed = 0
		st.lastEmit = now
		s.emit(ctx, sig, err, st.count, suppressed)
		return
	}
	st.suppressed++
}

func (s *Sampler) emit(ctx context.Context, sig string, err error, occurrence, suppressed uint64) {
	s.logger.ErrorContext(ctx, "operation failed",
		"signature", sig,
		"err", err,
		"occurrence", occurrence,
		"suppressed", suppressed,
	)
}
```

### The runnable demo

The demo uses a controllable fake clock to fire a burst of one signature: three
full lines (first=3), then a flood that is suppressed, then — after advancing the
clock past the interval — one throttled line reporting the suppressed count. Logs
go to stdout with time removed for determinism; the final line prints the exact
metric total.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"example.com/logsample"
)

type clock struct{ ns atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func main() {
	opts := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, opts))

	var clk clock
	s := logsample.NewSampler(logger, 3, time.Minute, clk.Now)
	ctx := context.Background()
	err := errors.New("connection refused")

	for range 5000 {
		s.Record(ctx, "db.dial", err) // 3 full lines, rest suppressed
	}
	clk.Advance(2 * time.Minute)
	s.Record(ctx, "db.dial", err) // one throttled line with the suppressed count

	fmt.Printf("metric total=%d\n", s.Total())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"ERROR","msg":"operation failed","signature":"db.dial","err":"connection refused","occurrence":1,"suppressed":0}
{"level":"ERROR","msg":"operation failed","signature":"db.dial","err":"connection refused","occurrence":2,"suppressed":0}
{"level":"ERROR","msg":"operation failed","signature":"db.dial","err":"connection refused","occurrence":3,"suppressed":0}
{"level":"ERROR","msg":"operation failed","signature":"db.dial","err":"connection refused","occurrence":5001,"suppressed":4997}
metric total=5001
```

### Tests

`TestMetricExactLogsSampled` fires 10000 errors with a frozen clock and asserts the
metric is exactly 10000 while the buffer holds exactly `first` lines — the storm is
counted precisely but logged cheaply. `TestThrottledLineReportsSuppressed` advances
the clock past the interval, fires once more, and asserts the new line reports the
suppressed count that accumulated. `TestConcurrentRecord` fires from many
goroutines under `-race` and asserts the total is exact.

Create `sampler_test.go`:

```go
package logsample

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"
)

type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func lines(buf *bytes.Buffer) []string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestMetricExactLogsSampled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var clk fakeClock
	s := NewSampler(slog.New(slog.NewJSONHandler(&buf, nil)), 3, time.Minute, clk.Now)

	err := errors.New("boom")
	const n = 10000
	for range n {
		s.Record(context.Background(), "sig", err) // clock frozen: nothing after first 3 emits
	}

	if s.Total() != n {
		t.Fatalf("metric total = %d, want %d (metric must be exact)", s.Total(), n)
	}
	if got := len(lines(&buf)); got != 3 {
		t.Fatalf("logged %d lines, want 3 (the first-N sample)", got)
	}
}

func TestThrottledLineReportsSuppressed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var clk fakeClock
	s := NewSampler(slog.New(slog.NewJSONHandler(&buf, nil)), 3, time.Minute, clk.Now)

	err := errors.New("boom")
	for range 1000 {
		s.Record(context.Background(), "sig", err)
	}
	clk.Advance(2 * time.Minute)
	s.Record(context.Background(), "sig", err) // throttled emit

	all := lines(&buf)
	last := all[len(all)-1]
	var m map[string]any
	if e := json.Unmarshal([]byte(last), &m); e != nil {
		t.Fatalf("bad json %q: %v", last, e)
	}
	// occurrences 4..1000 were suppressed (997), reported on this throttled line.
	if got := m["suppressed"].(float64); got != 997 {
		t.Fatalf("suppressed = %v, want 997", got)
	}
	if got := m["occurrence"].(float64); got != 1001 {
		t.Fatalf("occurrence = %v, want 1001", got)
	}
}

func TestConcurrentRecord(t *testing.T) {
	t.Parallel()
	var clk fakeClock
	s := NewSampler(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), 3, time.Minute, clk.Now)

	const goroutines, per = 8, 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range per {
				s.Record(context.Background(), "sig", errors.New("boom"))
			}
		}()
	}
	wg.Wait()

	if got, want := s.Total(), int64(goroutines*per); got != want {
		t.Fatalf("metric total = %d, want %d", got, want)
	}
}
```

## Review

The sampler is correct when the metric is exact and the logs are bounded.
`TestMetricExactLogsSampled` is the whole thesis in one assertion: 10000 recorded,
10000 counted, 3 logged. `TestThrottledLineReportsSuppressed` proves the throttled
line does not merely drop volume silently — it reports the 997 it suppressed, so
the operator can still see the true scale. `TestConcurrentRecord` under `-race`
proves the `atomic.Int64` total and per-signature mutexes are safe under
contention.

The two mistakes this prevents: logging unbounded on a hot path (the storm that
drowns the signal and inflates the bill) and, the opposite over-correction,
sampling the *metric* too — which would make the aggregate wrong and hide the
outage entirely. Sample the expensive pillar, keep the cheap one exact. The clock
is injected here because the throttle interval is precisely what is under test;
note the per-signature keying, so two different failures do not share one budget
and mask each other.

## Resources

- [`sync.Map`](https://pkg.go.dev/sync#Map) — the read-mostly concurrent map for per-signature state.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for the exact, lock-free metric total.
- [Zap: sampling](https://pkg.go.dev/go.uber.org/zap/zapcore#NewSamplerWithOptions) — a production logger's first-N-then-throttle sampler, the pattern this mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [10-error-severity-slo-burn.md](10-error-severity-slo-burn.md)
