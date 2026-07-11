# Exercise 2: Retry with Backoff and Jitter

A broker fails in two flavors: transient (a dropped connection, a momentary not-leader) that a retry will fix, and permanent (message too large, unknown topic) that a retry will never fix. This exercise builds the retry path that distinguishes them, retries transient failures with exponentially growing, randomly jittered backoff, and gives up immediately on permanent ones.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
retry.go             RetrySender, Config, Permanent, Backoff
  RetrySender.Send   loop up to Retries+1 attempts, sleeping the jittered backoff
  Permanent(err)     wrap an error so the loop stops retrying it
  Backoff(attempt)   base * 2^(attempt-1), scaled by a +-25% jitter factor
cmd/
  demo/
    main.go          a broker that fails twice then succeeds, and one permanent
retry_test.go        success after transient, exhaustion, permanent skip, bounds
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `RetrySender.Send`, the `Permanent` error wrapper, and `Backoff`.
- Test: a transient failure recovers and counts retries, an exhausted budget returns the last error, a permanent error is not retried, and `Backoff` stays inside its jitter window.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p retry-backoff-jitter/cmd/demo && cd retry-backoff-jitter
go mod init example.com/retry-backoff-jitter
```

### Why exponential, and why jitter is not optional

The retry loop runs at most `Retries + 1` attempts. Attempt zero is the original send. Before each later attempt it sleeps a backoff that doubles every time: `base`, `2*base`, `4*base`, and so on. Doubling is what keeps a struggling broker from being pounded; a fixed short interval would retry a down broker thousands of times a second and prevent it from recovering.

The part people skip is the jitter, and skipping it causes outages. Imagine a thousand producers that all lose their connection to the same broker at the same instant. With pure exponential backoff every one of them retries at exactly `base`, then exactly `2*base`, then exactly `4*base`: the retries arrive in synchronized waves, and the moment the broker comes back it is hit by a thousand simultaneous requests, the thundering herd, which knocks it over again. Multiplying each backoff by a random factor in `[0.75, 1.25]` smears those waves across a window so the load arrives spread out. The formula is:

```
exp    = base * 2^(attempt-1)
sleep  = exp * (0.75 + rand*0.5)      // exp, plus or minus 25%
```

The +-25% form here is "equal jitter": a deterministic exponential core with a bounded random perturbation, which keeps the backoff predictable enough to reason about while still decorrelating the herd. The AWS architecture analysis of "full jitter" and "decorrelated jitter" pushes the randomness further; the principle is identical and the win is the same.

### Permanent errors break the loop

Retrying an error that can never succeed is pure waste: the caller waits out the entire backoff schedule for an answer that was knowable on attempt zero. The loop must recognize a permanent failure and stop. The clean way to mark one is a wrapper type rather than a hard-coded list of sentinels. `Permanent(err)` wraps an error; `Send` checks `errors.As` for that wrapper and breaks immediately when it matches, while still returning the underlying error so the caller sees the real cause. This is exactly the pattern the popular `cenkalti/backoff` library exposes, and it composes with `errors.Is`/`errors.As` so a caller can still inspect the wrapped error normally.

The dispatch loop reads top to bottom as: for each attempt, sleep the backoff if this is a retry, call the broker, return on success, and on failure record the error and break if it is permanent or keep going otherwise. When the loop falls out the bottom it has exhausted the budget, so it records a failure and returns the last error seen.

Create `retry.go`:

```go
package retry

import (
	"errors"
	"math/rand"
	"sync"
	"time"
)

// TopicPartition identifies a log stream.
type TopicPartition struct {
	Topic     string
	Partition int32
}

// Record is one message inside a batch.
type Record struct {
	Key   []byte
	Value []byte
}

// RecordBatch is the unit the broker accepts.
type RecordBatch struct {
	TP        TopicPartition
	Records   []Record
	SizeBytes int
}

// BrokerSender is the network boundary. A real one dials the broker; tests and
// the demo inject a fake that fails on a schedule.
type BrokerSender interface {
	Send(batch *RecordBatch) error
}

// permanentError marks a failure the retry loop must not retry.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so RetrySender.Send stops retrying immediately. The
// underlying error is preserved for errors.Is / errors.As inspection.
func Permanent(err error) error { return &permanentError{err: err} }

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Config tunes the retry schedule.
type Config struct {
	// Retries is the number of retry attempts after the first try (default 3).
	Retries int
	// RetryBackoffMs is the base backoff before the first retry (default 100 ms).
	RetryBackoffMs int
}

func (c *Config) applyDefaults() {
	if c.Retries < 0 {
		c.Retries = 3
	}
	if c.RetryBackoffMs <= 0 {
		c.RetryBackoffMs = 100
	}
}

// Metrics counts what the retry loop did.
type Metrics struct {
	Attempts  int64 // total broker calls
	Retries   int64 // attempts after the first
	Successes int64 // batches eventually delivered
	Failures  int64 // batches that exhausted the budget or hit a permanent error
}

// RetrySender wraps a broker with a retry policy.
type RetrySender struct {
	cfg    Config
	broker BrokerSender

	mu      sync.Mutex
	rng     *rand.Rand
	metrics Metrics
}

// NewSender builds a RetrySender.
func NewSender(cfg Config, broker BrokerSender) *RetrySender {
	cfg.applyDefaults()
	return &RetrySender{
		cfg:    cfg,
		broker: broker,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Send delivers b, retrying transient failures up to Config.Retries times with
// exponential backoff and +-25% jitter. Permanent errors are not retried.
func (s *RetrySender) Send(b *RecordBatch) error {
	var lastErr error
	for attempt := 0; attempt <= s.cfg.Retries; attempt++ {
		if attempt > 0 {
			time.Sleep(s.Backoff(attempt))
			s.bump(func(m *Metrics) { m.Retries++ })
		}
		s.bump(func(m *Metrics) { m.Attempts++ })
		err := s.broker.Send(b)
		if err == nil {
			s.bump(func(m *Metrics) { m.Successes++ })
			return nil
		}
		lastErr = err
		if isPermanent(err) {
			break
		}
	}
	s.bump(func(m *Metrics) { m.Failures++ })
	return lastErr
}

// Backoff returns the sleep before attempt n (1-indexed): base * 2^(n-1),
// scaled by a uniform jitter factor in [0.75, 1.25].
func (s *RetrySender) Backoff(n int) time.Duration {
	base := time.Duration(s.cfg.RetryBackoffMs) * time.Millisecond
	exp := base << uint(n-1)
	s.mu.Lock()
	factor := 0.75 + s.rng.Float64()*0.5
	s.mu.Unlock()
	return time.Duration(float64(exp) * factor)
}

// Metrics returns a snapshot of the counters.
func (s *RetrySender) Metrics() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics
}

func (s *RetrySender) bump(fn func(*Metrics)) {
	s.mu.Lock()
	fn(&s.metrics)
	s.mu.Unlock()
}
```

### The runnable demo

The demo wires up two brokers. The first fails twice with a transient error and then succeeds, so the sender retries and delivers on the third attempt. The second returns a permanent error, so the sender tries exactly once and returns. With `RetryBackoffMs: 1` the sleeps are sub-millisecond, so the run is fast; the output is the broker's per-attempt log followed by the metrics, both deterministic because the demo is single-threaded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/retry-backoff-jitter"
)

// flakyBroker fails its first failFirst calls, then succeeds.
type flakyBroker struct {
	calls     int
	failFirst int
	failErr   error
}

func (b *flakyBroker) Send(batch *retry.RecordBatch) error {
	b.calls++
	if b.calls <= b.failFirst {
		fmt.Printf("broker: attempt %d -> %v\n", b.calls, b.failErr)
		return b.failErr
	}
	fmt.Printf("broker: attempt %d -> ok\n", b.calls)
	return nil
}

func main() {
	batch := &retry.RecordBatch{
		TP:        retry.TopicPartition{Topic: "orders", Partition: 0},
		Records:   []retry.Record{{Value: []byte("a")}, {Value: []byte("b")}},
		SizeBytes: 2,
	}

	transient := &flakyBroker{failFirst: 2, failErr: errors.New("broker unavailable")}
	s := retry.NewSender(retry.Config{Retries: 5, RetryBackoffMs: 1}, transient)
	if err := s.Send(batch); err != nil {
		fmt.Println("transient result:", err)
	} else {
		fmt.Println("transient result: delivered")
	}

	permanent := &flakyBroker{failFirst: 99, failErr: retry.Permanent(errors.New("message too large"))}
	s2 := retry.NewSender(retry.Config{Retries: 5, RetryBackoffMs: 1}, permanent)
	fmt.Println("permanent result:", s2.Send(batch))

	m := s.Metrics()
	fmt.Printf("transient: attempts=%d retries=%d successes=%d failures=%d\n",
		m.Attempts, m.Retries, m.Successes, m.Failures)
	m2 := s2.Metrics()
	fmt.Printf("permanent: attempts=%d retries=%d successes=%d failures=%d\n",
		m2.Attempts, m2.Retries, m2.Successes, m2.Failures)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
broker: attempt 1 -> broker unavailable
broker: attempt 2 -> broker unavailable
broker: attempt 3 -> ok
transient result: delivered
broker: attempt 1 -> message too large
permanent result: message too large
transient: attempts=3 retries=2 successes=1 failures=0
permanent: attempts=1 retries=0 successes=0 failures=1
```

### Tests

The tests pin the four behaviors. `TestRecoversAfterTransient` proves two failures then a success yields delivery with exactly two retries counted. `TestExhaustsBudget` proves a broker that never recovers returns the last error after `Retries + 1` attempts. `TestPermanentNotRetried` proves a permanent error costs exactly one attempt. `TestBackoffBounds` proves every backoff for a given attempt lands inside its `[0.75, 1.25] * base * 2^(n-1)` window, and that later attempts never overlap earlier ones.

Create `retry_test.go`:

```go
package retry

import (
	"errors"
	"sync"
	"testing"
)

type scriptedBroker struct {
	mu      sync.Mutex
	calls   int
	failN   int
	failErr error
}

func (b *scriptedBroker) Send(_ *RecordBatch) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls <= b.failN {
		return b.failErr
	}
	return nil
}

func batch() *RecordBatch {
	return &RecordBatch{TP: TopicPartition{Topic: "t", Partition: 0}, Records: []Record{{Value: []byte("x")}}, SizeBytes: 1}
}

func TestRecoversAfterTransient(t *testing.T) {
	t.Parallel()

	broker := &scriptedBroker{failN: 2, failErr: errors.New("unavailable")}
	s := NewSender(Config{Retries: 3, RetryBackoffMs: 1}, broker)

	if err := s.Send(batch()); err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}
	m := s.Metrics()
	if m.Attempts != 3 || m.Retries != 2 || m.Successes != 1 || m.Failures != 0 {
		t.Errorf("metrics = %+v, want attempts=3 retries=2 successes=1 failures=0", m)
	}
}

func TestExhaustsBudget(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("still down")
	broker := &scriptedBroker{failN: 99, failErr: sentinel}
	s := NewSender(Config{Retries: 2, RetryBackoffMs: 1}, broker)

	if err := s.Send(batch()); !errors.Is(err, sentinel) {
		t.Fatalf("Send() = %v, want sentinel", err)
	}
	m := s.Metrics()
	if m.Attempts != 3 || m.Retries != 2 || m.Failures != 1 {
		t.Errorf("metrics = %+v, want attempts=3 retries=2 failures=1", m)
	}
}

func TestPermanentNotRetried(t *testing.T) {
	t.Parallel()

	cause := errors.New("message too large")
	broker := &scriptedBroker{failN: 99, failErr: Permanent(cause)}
	s := NewSender(Config{Retries: 5, RetryBackoffMs: 1}, broker)

	err := s.Send(batch())
	if !errors.Is(err, cause) {
		t.Fatalf("Send() = %v, want to wrap cause", err)
	}
	m := s.Metrics()
	if m.Attempts != 1 || m.Retries != 0 || m.Failures != 1 {
		t.Errorf("metrics = %+v, want attempts=1 retries=0 failures=1", m)
	}
}

func TestBackoffBounds(t *testing.T) {
	t.Parallel()

	s := NewSender(Config{RetryBackoffMs: 100}, &scriptedBroker{})
	for n := 1; n <= 4; n++ {
		expMs := 100 * (1 << uint(n-1))
		lo := float64(expMs) * 0.75
		hi := float64(expMs) * 1.25
		for i := 0; i < 1000; i++ {
			got := float64(s.Backoff(n).Milliseconds())
			if got < lo-1 || got > hi+1 {
				t.Fatalf("attempt %d: Backoff = %.0fms, want within [%.0f, %.0f]", n, got, lo, hi)
			}
		}
	}
}

func TestConcurrentSendIsRaceFree(t *testing.T) {
	t.Parallel()

	broker := &scriptedBroker{}
	s := NewSender(Config{Retries: 1, RetryBackoffMs: 1}, broker)

	done := make(chan struct{})
	for g := 0; g < 16; g++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 50; i++ {
				_ = s.Send(batch())
				_ = s.Backoff(2)
			}
		}()
	}
	for g := 0; g < 16; g++ {
		<-done
	}
}
```

## Review

The retry path is correct when transient and permanent failures take visibly different paths. Confirm a transient failure retries until success or budget exhaustion and counts each retry, while a permanent failure costs exactly one attempt and still surfaces its underlying cause through `errors.Is`. Confirm the backoff for attempt `n` always lands in `[0.75, 1.25] * base * 2^(n-1)`, which both grows the delay and decorrelates it; because `1.25 * exp(n) < 0.75 * exp(n+1)`, the windows never overlap and a later retry always waits longer than an earlier one.

The mistakes to avoid. A flat loop that retries every error burns the whole budget on a message-too-large that was hopeless from attempt zero; the `isPermanent` check and `break` are what prevent it. Dropping the jitter and retrying at exact exponential instants recreates the thundering herd the backoff was meant to tame. Sharing one `*rand.Rand` across goroutines without a lock is a data race that `-race` will flag, which is why `Backoff` takes the mutex around `rng.Float64()`. The concurrency test exercises that last point directly.

## Resources

- [Exponential Backoff and Jitter (AWS Architecture Blog)](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — the measured comparison of no jitter, full jitter, and decorrelated jitter that motivates the random factor.
- [`cenkalti/backoff`](https://pkg.go.dev/github.com/cenkalti/backoff/v4) — the widely used Go backoff library and its `Permanent` wrapper, the pattern this exercise mirrors.
- [`math/rand`](https://pkg.go.dev/math/rand) — `rand.New`, `Source`, and why a shared `*rand.Rand` needs external synchronization.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-batch-compression.md](03-batch-compression.md)
