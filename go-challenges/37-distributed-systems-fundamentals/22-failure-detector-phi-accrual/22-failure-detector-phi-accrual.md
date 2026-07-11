# 22. Failure Detector: Phi Accrual

The phi accrual failure detector (Hayashibara et al., 2004) replaces the binary alive/dead judgment of fixed-timeout detectors with a continuous suspicion value called phi. The hard part is that phi is computed from a statistical model of heartbeat inter-arrival times, so the detector adapts to changing network conditions rather than being tuned for a worst case. The application picks its own threshold, trading detection speed against false-positive tolerance.

```text
phidetector/
  go.mod
  detector.go
  detector_test.go
  cmd/demo/main.go
```

## Concepts

### Why Fixed Timeouts Fail

A fixed-timeout detector declares a peer dead when no heartbeat arrives within T seconds. Tuning T is a dilemma: a small T causes false positives during network jitter; a large T delays failure detection. Because network conditions change (GC pauses, load spikes, datacenter cross-links), any single T is wrong some of the time.

Cassandra, Akka, and similar systems replace T with phi: a value derived from how long the peer has been silent relative to what is statistically expected given its recent heartbeat history.

### The Heartbeat History

The detector maintains a sliding window of the last N inter-arrival times (the gap between successive heartbeats from one peer). The window has a fixed capacity; when full, the oldest value is discarded.

From the window the detector computes:
- mean (mu): average inter-arrival time
- variance: spread of the inter-arrival times, capturing network jitter

### The Normal CDF and Erfc

Given the window statistics, the detector models inter-arrival times as normally distributed with the observed mean and standard deviation. The survival probability — the chance that a heartbeat from a live peer would still not have arrived by time t — is:

```
P_later(t) = 0.5 * erfc((t - mu) / (sqrt(2) * sigma))
```

Using `math.Erfc` (the complementary error function) is critical here: computing `1 - erf(x)` suffers catastrophic cancellation for large x because `erf(x)` approaches 1.0 and the subtraction loses all precision. `math.Erfc(x)` is computed directly from the tail and remains accurate for very large x.

### The Phi Value

Phi is the negative base-10 logarithm of the survival probability:

```
phi(t) = -log10(P_later(t))
```

When t equals mu, `P_later` is 0.5, so phi is approximately 0.3. As silence grows, phi increases smoothly. The original paper reports that phi = 1 corresponds to a mistake rate of 10%, phi = 2 to 1%, phi = 8 to one in 100 million. Cassandra's default threshold is 8.

### Threshold and the Application Contract

The application calls `IsAvailable(peer, threshold)`, which returns false when phi exceeds the threshold. Different subsystems can use different tolerances: a read path may accept threshold 8, while a leader-election path may require 16.

### Bootstrapping

Before the window has enough samples, the detector seeds the ring buffer with a configured initial interval (typically the expected heartbeat period). This prevents a cold-start false positive before the window fills.

### Failure Modes

- Window too small: variance estimate is noisy; small bursts of jitter appear fatal.
- No sigma guard: if all intervals are identical (sigma = 0), the division in the erfc argument overflows. Guard with a minimum sigma.
- Clock skew: phi is computed against wall-clock elapsed time. Go's `time.Now()` uses a monotonic reading, so backward-clock adjustments do not cause negative elapsed values.
- No heartbeat recorded: phi cannot be computed without at least one heartbeat. Return `ErrNoHeartbeat` before computing phi when no heartbeat has been seen.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/phidetector/cmd/demo
cd ~/go-exercises/phidetector
go mod init example.com/phidetector
```

This is a library, not a program: the package is `phidetector`, not `main`. You verify it with `go test`.

### Exercise 1: Heartbeat History and Statistics

Create `detector.go`:

```go
package phidetector

import (
	"errors"
	"math"
	"sync"
	"time"
)

// ErrNoPeer is returned when Phi or IsAvailable is called for an unknown peer.
var ErrNoPeer = errors.New("phidetector: peer not registered")

// ErrNoHeartbeat is returned when phi cannot be computed because no heartbeat
// has been recorded for the peer yet.
var ErrNoHeartbeat = errors.New("phidetector: no heartbeat recorded")

const (
	minSigma      = 0.01 // 10 ms minimum standard deviation guard
	defaultWindow = 1000 // sliding window capacity
)

// history tracks the inter-arrival times for one peer.
type history struct {
	intervals []float64 // seconds; ring buffer
	cap       int
	head      int // next write index
	size      int // number of valid entries
	lastAt    time.Time
}

func newHistory(capacity int, seed float64) *history {
	h := &history{
		intervals: make([]float64, capacity),
		cap:       capacity,
	}
	// Bootstrap with seed values so cold start does not produce a false positive.
	for i := 0; i < capacity; i++ {
		h.intervals[i] = seed
	}
	h.size = capacity
	return h
}

// record adds a new inter-arrival time sample.
func (h *history) record(now time.Time) {
	if !h.lastAt.IsZero() {
		dt := now.Sub(h.lastAt).Seconds()
		if dt > 0 {
			h.intervals[h.head] = dt
			h.head = (h.head + 1) % h.cap
			if h.size < h.cap {
				h.size++
			}
		}
	}
	h.lastAt = now
}

// stats returns the mean and standard deviation of the recorded intervals.
func (h *history) stats() (mean, sigma float64) {
	n := h.size
	if n == 0 {
		return 0, minSigma
	}
	sum := 0.0
	for _, v := range h.intervals[:n] {
		sum += v
	}
	mean = sum / float64(n)
	variance := 0.0
	for _, v := range h.intervals[:n] {
		d := v - mean
		variance += d * d
	}
	variance /= float64(n)
	sigma = math.Sqrt(variance)
	if sigma < minSigma {
		sigma = minSigma
	}
	return mean, sigma
}

// phi computes the suspicion level for the given elapsed time since the last
// heartbeat, using the survival function of the normal distribution.
//
// phi = -log10(0.5 * erfc((elapsed-mean) / (sqrt(2)*sigma)))
//
// math.Erfc is used instead of 1-math.Erf to avoid catastrophic cancellation
// in the tail of the distribution where erf(x) is very close to 1.
//
// See: Hayashibara et al., "The phi Accrual Failure Detector" (2004).
func (h *history) phi(elapsed float64) float64 {
	mean, sigma := h.stats()
	x := (elapsed - mean) / (math.Sqrt2 * sigma)
	// survival = P(inter-arrival > elapsed) = 0.5 * erfc(x)
	survival := 0.5 * math.Erfc(x)
	if survival < 1e-300 {
		survival = 1e-300
	}
	return -math.Log10(survival)
}

// Detector tracks heartbeats from multiple peers and provides phi-based
// availability checks.
type Detector struct {
	mu             sync.Mutex
	peers          map[string]*history
	windowCapacity int
	seedInterval   float64 // seconds
}

// New creates a Detector with the given sliding-window capacity and bootstrap
// seed interval (the expected heartbeat period).
func New(windowCapacity int, seedInterval time.Duration) *Detector {
	if windowCapacity <= 0 {
		windowCapacity = defaultWindow
	}
	return &Detector{
		peers:          make(map[string]*history),
		windowCapacity: windowCapacity,
		seedInterval:   seedInterval.Seconds(),
	}
}

// Register pre-creates a history for peer so it is known before the first
// heartbeat. Calling Register after the first Heartbeat is a no-op.
func (d *Detector) Register(peer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.peers[peer]; !ok {
		d.peers[peer] = newHistory(d.windowCapacity, d.seedInterval)
	}
}

// Heartbeat records a heartbeat for peer at the current wall-clock time.
// If the peer is not yet known it is registered automatically.
func (d *Detector) Heartbeat(peer string) {
	d.HeartbeatAt(peer, time.Now())
}

// HeartbeatAt records a heartbeat at an explicit time (useful for testing).
func (d *Detector) HeartbeatAt(peer string, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.peers[peer]
	if !ok {
		h = newHistory(d.windowCapacity, d.seedInterval)
		d.peers[peer] = h
	}
	h.record(at)
}

// Phi returns the current suspicion level for peer. A low phi (< 1) indicates
// the peer is almost certainly alive. A high phi (> 8) indicates high
// suspicion of failure. Cassandra uses a threshold of 8.
//
// Returns ErrNoPeer if the peer has never been seen, and ErrNoHeartbeat if
// the peer was registered but no heartbeat has been recorded.
func (d *Detector) Phi(peer string) (float64, error) {
	return d.PhiAt(peer, time.Now())
}

// PhiAt computes phi at an explicit time (useful for testing).
func (d *Detector) PhiAt(peer string, at time.Time) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.peers[peer]
	if !ok {
		return 0, ErrNoPeer
	}
	if h.lastAt.IsZero() {
		return 0, ErrNoHeartbeat
	}
	elapsed := at.Sub(h.lastAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	return h.phi(elapsed), nil
}

// IsAvailable returns true if the peer's phi is below threshold.
// Returns false (with no error) when ErrNoPeer or ErrNoHeartbeat applies,
// because a peer we have never heard from cannot be considered available.
func (d *Detector) IsAvailable(peer string, threshold float64) bool {
	phi, err := d.Phi(peer)
	if err != nil {
		return false
	}
	return phi < threshold
}

// isAvailableAt is the time-controllable version of IsAvailable, used in tests.
func (d *Detector) isAvailableAt(peer string, threshold float64, at time.Time) bool {
	phi, err := d.PhiAt(peer, at)
	if err != nil {
		return false
	}
	return phi < threshold
}
```

The two exported error sentinels (`ErrNoPeer`, `ErrNoHeartbeat`) let callers distinguish "never seen" from "registered but silent", which affects how an application responds.

The `isAvailableAt` unexported method gives tests deterministic time control without exposing a wall-clock dependency in the public API.

### Exercise 2: Test the Detector Contract

Create `detector_test.go`:

```go
package phidetector

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// seedInterval used throughout the tests: 1 s expected heartbeat period.
const testSeed = time.Second

// feedVariableHeartbeats sends n heartbeats to peer d with alternating
// intervals of shortInterval and longInterval, starting at base. It returns
// the time of the last heartbeat. The alternating pattern ensures the history
// has non-trivial variance so phi grows smoothly across a useful range.
func feedVariableHeartbeats(d *Detector, peer string, base time.Time, n int, short, long time.Duration) time.Time {
	t := base
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			t = t.Add(short)
		} else {
			t = t.Add(long)
		}
		d.HeartbeatAt(peer, t)
	}
	return t
}

func TestPhiGrowsWithSilence(t *testing.T) {
	t.Parallel()

	d := New(100, testSeed)
	base := time.Now()

	// Alternating 500ms/1500ms heartbeats: mean=1s, sigma~500ms.
	// With sigma that large, phi grows smoothly from 0.3 at 1s to well above 8 at 8s.
	last := feedVariableHeartbeats(d, "peer1", base, 40, 500*time.Millisecond, 1500*time.Millisecond)

	phi1, err := d.PhiAt("peer1", last.Add(1*time.Second))
	if err != nil {
		t.Fatalf("PhiAt(1s): %v", err)
	}
	phi2, err := d.PhiAt("peer1", last.Add(3*time.Second))
	if err != nil {
		t.Fatalf("PhiAt(3s): %v", err)
	}
	phi3, err := d.PhiAt("peer1", last.Add(8*time.Second))
	if err != nil {
		t.Fatalf("PhiAt(8s): %v", err)
	}

	if phi1 >= phi2 {
		t.Errorf("phi(1s)=%f >= phi(3s)=%f; want strictly increasing", phi1, phi2)
	}
	if phi2 >= phi3 {
		t.Errorf("phi(3s)=%f >= phi(8s)=%f; want strictly increasing", phi2, phi3)
	}
}

func TestPhiLowWhenHeartbeatsAreTimely(t *testing.T) {
	t.Parallel()

	d := New(100, testSeed)
	base := time.Now()

	// Regular 1-second heartbeats; seed dominates mean.
	for i := 0; i < 50; i++ {
		d.HeartbeatAt("peer1", base.Add(time.Duration(i)*time.Second))
	}

	// 200 ms after the last heartbeat: well within the mean interval.
	last := base.Add(49 * time.Second)
	phi, err := d.PhiAt("peer1", last.Add(200*time.Millisecond))
	if err != nil {
		t.Fatalf("PhiAt: %v", err)
	}
	if phi >= 1.0 {
		t.Errorf("phi=%f at 0.2*mean; want < 1.0 (peer is healthy)", phi)
	}
}

func TestPhiHighAfterLongSilence(t *testing.T) {
	t.Parallel()

	d := New(100, testSeed)
	base := time.Now()

	// Alternating heartbeats to build realistic variance.
	last := feedVariableHeartbeats(d, "peer1", base, 40, 500*time.Millisecond, 1500*time.Millisecond)

	// 8 seconds of silence with mean=1s, sigma~0.5s: phi should exceed 8.
	phi, err := d.PhiAt("peer1", last.Add(8*time.Second))
	if err != nil {
		t.Fatalf("PhiAt: %v", err)
	}
	if phi < 8.0 {
		t.Errorf("phi=%f after 8s silence; want >= 8 (suspect dead)", phi)
	}
}

func TestErrNoPeerForUnknownPeer(t *testing.T) {
	t.Parallel()

	d := New(10, testSeed)
	_, err := d.Phi("unknown")
	if !errors.Is(err, ErrNoPeer) {
		t.Errorf("err = %v, want ErrNoPeer", err)
	}
}

func TestErrNoHeartbeatAfterRegister(t *testing.T) {
	t.Parallel()

	d := New(10, testSeed)
	d.Register("peer2")
	_, err := d.Phi("peer2")
	if !errors.Is(err, ErrNoHeartbeat) {
		t.Errorf("err = %v, want ErrNoHeartbeat", err)
	}
}

func TestIsAvailableFalseForUnknownPeer(t *testing.T) {
	t.Parallel()

	d := New(10, testSeed)
	if d.IsAvailable("unknown", 8.0) {
		t.Error("IsAvailable: unknown peer should not be available")
	}
}

func TestIsAvailableTrueForHealthyPeer(t *testing.T) {
	t.Parallel()

	d := New(100, testSeed)
	base := time.Now()

	last := feedVariableHeartbeats(d, "peer1", base, 40, 500*time.Millisecond, 1500*time.Millisecond)

	// 200 ms after last heartbeat: phi is well below 8.
	if !d.isAvailableAt("peer1", 8.0, last.Add(200*time.Millisecond)) {
		t.Error("IsAvailable: healthy peer should be available at threshold 8")
	}
}

func TestIsAvailableFalseAfterLongSilence(t *testing.T) {
	t.Parallel()

	d := New(100, testSeed)
	base := time.Now()

	last := feedVariableHeartbeats(d, "peer1", base, 40, 500*time.Millisecond, 1500*time.Millisecond)

	// 8 seconds of silence: phi exceeds 8.
	if d.isAvailableAt("peer1", 8.0, last.Add(8*time.Second)) {
		t.Error("IsAvailable: silent peer should not be available at threshold 8")
	}
}

func TestHeartbeatAutoRegisters(t *testing.T) {
	t.Parallel()

	d := New(10, testSeed)
	d.Heartbeat("peer3")
	_, err := d.Phi("peer3")
	if errors.Is(err, ErrNoPeer) {
		t.Error("Heartbeat should auto-register the peer")
	}
}

func TestHistoryPhiIsFinite(t *testing.T) {
	t.Parallel()

	h := newHistory(10, 1.0)
	base := time.Now()
	for i := 0; i < 10; i++ {
		h.record(base.Add(time.Duration(i) * time.Second))
	}
	phi := h.phi(5.0)
	if math.IsNaN(phi) || math.IsInf(phi, 0) {
		t.Errorf("phi = %v; want finite value", phi)
	}
}

func TestWindowCapacityLimitsHistory(t *testing.T) {
	t.Parallel()

	cap := 5
	h := newHistory(cap, 1.0)
	base := time.Now()

	// Feed cap+10 heartbeats; only the most recent cap inter-arrivals should count.
	for i := 0; i < cap+10; i++ {
		h.record(base.Add(time.Duration(i) * time.Second))
	}

	if h.size != cap {
		t.Errorf("history.size = %d, want %d", h.size, cap)
	}
}

// ExampleNew demonstrates basic detector usage.
func ExampleNew() {
	d := New(100, time.Second)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// 40 alternating-interval heartbeats: mean=1s, sigma~0.5s.
	last := base
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			last = last.Add(500 * time.Millisecond)
		} else {
			last = last.Add(1500 * time.Millisecond)
		}
		d.HeartbeatAt("nodeA", last)
	}

	// 200 ms after last heartbeat: phi < 1 (peer is healthy).
	phi, _ := d.PhiAt("nodeA", last.Add(200*time.Millisecond))
	fmt.Println(phi < 1.0)
	// Output: true
}
```

Your turn: add `TestHistoryStatsMeanApproximatesInterval` — create a `newHistory(50, 1.0)`, call `record` 50 times at exactly 1-second intervals starting from `time.Now()`, then call `stats()` and assert that `mean` is within 0.01 of 1.0.

### Exercise 3: Command-line Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/phidetector"
)

func main() {
	const (
		heartbeatInterval = time.Second
		threshold         = 8.0
		beats             = 40
	)

	d := phidetector.New(100, heartbeatInterval)
	base := time.Now()

	// Warm up the detector with alternating-interval heartbeats (mean=1s, sigma~0.5s).
	fmt.Println("Warming up with 40 alternating-interval heartbeats...")
	t := base
	for i := 0; i < beats; i++ {
		if i%2 == 0 {
			t = t.Add(500 * time.Millisecond)
		} else {
			t = t.Add(1500 * time.Millisecond)
		}
		d.HeartbeatAt("nodeA", t)
	}
	last := t

	fmt.Println("\nPhi values after last heartbeat:")
	for _, delay := range []time.Duration{
		200 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
		10 * time.Second,
	} {
		phi, _ := d.PhiAt("nodeA", last.Add(delay))
		avail := phi < threshold
		fmt.Printf("  t+%-8s  phi=%7.4f  available=%v\n", delay, phi, avail)
	}
}
```

Run the demo with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Using `1 - math.Erf(x)` Instead of `math.Erfc(x)`

Wrong: computing `survival := 1 - 0.5*(1+math.Erf(x))` for large x. When x exceeds about 6, `math.Erf(x)` returns exactly 1.0 in float64 and the subtraction yields 0.0, so phi becomes +Inf immediately.

Fix: use `survival := 0.5 * math.Erfc(x)`. `math.Erfc` is implemented to be accurate in the tail; it returns a small positive number rather than cancelling to zero.

### Forgetting the Minimum Sigma Guard

Wrong: computing `h.stats()` without a minimum sigma when all recorded intervals are identical (variance = 0). The erfc argument grows without bound and `math.Erfc` returns 0.0, producing phi = +Inf at any elapsed > mean.

Fix: clamp sigma to a minimum (here 10 ms). The guard has negligible effect when real network jitter is larger than 10 ms, which is almost always true in production.

### Using `h.intervals` Instead of `h.intervals[:h.size]`

Wrong: ranging over `h.intervals` (the full backing array) before the ring has filled. The unwritten suffix still holds bootstrap seed values, biasing the mean and variance.

Fix: always range over `h.intervals[:h.size]`. Once `h.size == h.cap`, the entire ring has been overwritten with real samples.

### Setting Threshold Too Low for the Observed Jitter

Wrong: using threshold 2.0 on a cluster where network jitter causes sigma ~500ms. With mean=1s and sigma=500ms, phi reaches 2 routinely at elapsed = 1.5s — well within normal operation — so the detector fires false positives on every jitter spike.

Fix: measure the network's jitter profile in staging and pick a threshold that phi reaches only when silence is several standard deviations beyond the mean. Cassandra's default of 8 matches a one-in-100-million false-positive rate under the normal model.

## Verification

From `~/go-exercises/phidetector`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the primary verification; the demo is supplementary.

## Summary

- Phi replaces a binary timeout with a continuous suspicion level derived from the statistical distribution of heartbeat inter-arrival times.
- The detector maintains a fixed-capacity sliding window of inter-arrival times; older samples are discarded as the ring wraps.
- Phi = -log10(0.5 * erfc((t - mean) / (sqrt(2) * sigma))); it grows smoothly as silence extends past the mean.
- Use `math.Erfc` rather than `1 - math.Erf` to avoid catastrophic cancellation in the distribution tail.
- Two sentinel errors (`ErrNoPeer`, `ErrNoHeartbeat`) let the application distinguish "never seen" from "registered but silent".
- The application chooses the threshold per code path: 8 is Cassandra's production default; 16 is near-zero false positive.
- Bootstrap the window with the expected heartbeat interval to avoid cold-start false positives.

## What's Next

Next: [Quorum-Based Replication](../23-quorum-based-replication/23-quorum-based-replication.md).

## Resources

- [The Phi Accrual Failure Detector (Hayashibara et al., 2004)](https://www.researchgate.net/publication/29682135_The_ph_accrual_failure_detector) — the original paper defining phi and the normal distribution model
- [Cassandra Architecture: Failure Detection](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html#failure-detection) — production use with default threshold 8
- [pkg.go.dev/math](https://pkg.go.dev/math) — `math.Erfc`, `math.Log10`, `math.Sqrt2` used in the phi computation
- [Unreliable Failure Detectors for Reliable Distributed Systems (Chandra & Toueg, 1996)](https://dl.acm.org/doi/10.1145/226643.226647) — foundational theory of completeness and accuracy properties
- [Go sync package](https://pkg.go.dev/sync) — `sync.Mutex` for concurrent peer map access
