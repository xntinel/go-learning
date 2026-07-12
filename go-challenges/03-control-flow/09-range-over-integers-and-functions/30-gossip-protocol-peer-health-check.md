# Exercise 30: Gossip Protocol Peer Health Checker — Probing Replicas with Jittered Exponential Backoff

**Nivel: Intermedio** — validacion rapida (un test corto).

A cluster of peers that all decide "who is down" independently -- no
coordinator, no consensus round -- has to probe carefully: always checking
peers in the same order turns the first peer in the list into an
unintentional priority target under load, and retrying a slow-to-recover
peer on a fixed schedule synchronizes every prober in the cluster into
hammering it in lockstep the moment their backoff windows line up. This
exercise builds a `Prober` that shuffles probe order per round and adds
random jitter to exponential backoff, turning both failure modes into
explicit, tested properties of the code. This exercise is an independent
module with its own `go mod init`.

## What you'll build

```text
gossip/                    independent module: example.com/gossip-protocol-peer-health-check
  go.mod                    module example.com/gossip-protocol-peer-health-check
  gossip.go                 Prober, New, ProbeResult, Probe
  cmd/
    demo/
      main.go               runnable demo: 4 peers, mixed success attempts
  gossip_test.go             determinism, zero-backoff success, exhausted retries, early-stop, panics
```

Implement: `New(maxAttempts int, baseBackoff time.Duration, seed1, seed2 uint64) *Prober` and `(*Prober) Probe(peers []string, probeFn func(peer string, attempt int) bool) iter.Seq[ProbeResult]` yielding one `ProbeResult{Peer, Attempt, Alive, Backoff}` per peer per round.
Test: the same seed reproduces the same shuffled order and the same jitter every run; a peer that answers on attempt 1 has zero backoff; a peer that never answers is marked down after exactly `maxAttempts` tries; a consumer break stops probing the remaining peers; `New` panics on a non-positive attempt count or backoff.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/30-gossip-protocol-peer-health-check/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/30-gossip-protocol-peer-health-check
go mod edit -go=1.24
```

Two design choices keep this exercise both realistic and testable at the
same time. First, `Probe` seeds its `*rand.Rand` explicitly from caller-supplied
values instead of reading a global, unseeded random source -- a production
prober would derive those seeds from real entropy once at startup, but
seeding deterministically here is what makes "the same seed reproduces the
same round" a provable test property instead of a flaky one. Second, `Probe`
yields exactly one `ProbeResult` per peer, after its retries are already
exhausted or it has answered -- not one result per attempt. A consumer
aggregating cluster health wants "is `peer-c` up, and how many tries did it
cost," not a play-by-play of every failed attempt; folding the retry loop
inside the iterator and only yielding the round's final outcome is what
makes that the natural shape of the API instead of something the caller has
to reconstruct from a stream of per-attempt events.

Create `gossip.go`:

```go
package gossip

import (
	"iter"
	"math/rand/v2"
	"time"
)

// ProbeResult is the final outcome of probing one peer for one gossip
// round: how many attempts it took, whether the peer ultimately answered,
// and the total backoff time that would have been waited between attempts.
type ProbeResult struct {
	Peer    string
	Attempt int
	Alive   bool
	Backoff time.Duration
}

// Prober runs gossip-style health probes: peers are visited in a
// randomized order each round (so no single peer is always probed first,
// which would make it an unintentional priority target under load), and a
// peer that does not answer is retried with exponential backoff and jitter
// before being marked down.
type Prober struct {
	maxAttempts int
	baseBackoff time.Duration
	rng         *rand.Rand
}

// New creates a Prober. maxAttempts and baseBackoff must be positive: zero
// attempts could never produce a result, and a zero or negative backoff
// would retry as fast as the CPU allows, defeating the point of backing off
// at all. seed1/seed2 seed the probe order and jitter deterministically --
// callers that want true randomness should derive the seeds from a fresh
// source of entropy themselves.
func New(maxAttempts int, baseBackoff time.Duration, seed1, seed2 uint64) *Prober {
	if maxAttempts < 1 {
		panic("gossip: maxAttempts must be >= 1")
	}
	if baseBackoff <= 0 {
		panic("gossip: baseBackoff must be > 0")
	}
	return &Prober{
		maxAttempts: maxAttempts,
		baseBackoff: baseBackoff,
		rng:         rand.New(rand.NewPCG(seed1, seed2)),
	}
}

// Probe runs one gossip round over peers, calling probeFn(peer, attempt)
// for each attempt. It yields exactly one ProbeResult per peer -- the
// round's final outcome -- never one result per attempt, because a
// consumer aggregating cluster health cares about "is this peer up" and
// "how many retries did it cost," not a play-by-play of every failed
// attempt. Peers are visited in a random order (shuffled once per Probe
// call) and, on failure, each retry waits baseBackoff*2^(attempt-1) plus a
// random jitter up to half that backoff -- the jitter is what keeps a
// cluster of gossiping peers from retrying in lockstep and re-flooding a
// recovering peer with simultaneous probes the instant its backoff windows
// happen to align.
func (p *Prober) Probe(peers []string, probeFn func(peer string, attempt int) bool) iter.Seq[ProbeResult] {
	order := make([]string, len(peers))
	copy(order, peers)
	p.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	return func(yield func(ProbeResult) bool) {
		for _, peer := range order {
			alive := false
			var total time.Duration
			lastAttempt := 0
			for a := 1; a <= p.maxAttempts; a++ {
				lastAttempt = a
				if probeFn(peer, a) {
					alive = true
					break
				}
				backoff := p.baseBackoff * time.Duration(1<<uint(a-1))
				jitter := time.Duration(p.rng.Int64N(int64(backoff)/2 + 1))
				total += backoff + jitter
			}
			result := ProbeResult{Peer: peer, Attempt: lastAttempt, Alive: alive, Backoff: total}
			if !yield(result) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/gossip-protocol-peer-health-check"
)

func main() {
	peers := []string{"peer-a", "peer-b", "peer-c", "peer-d"}

	successAt := map[string]int{
		"peer-a": 1,
		"peer-b": 2,
		"peer-c": 3,
		"peer-d": 0, // never succeeds
	}
	probe := func(peer string, attempt int) bool {
		want := successAt[peer]
		return want != 0 && attempt >= want
	}

	p := gossip.New(3, 100*time.Millisecond, 1, 2)
	for result := range p.Probe(peers, probe) {
		fmt.Printf("peer=%-7s attempts=%d alive=%-5v backoff=%v\n", result.Peer, result.Attempt, result.Alive, result.Backoff)
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
peer=peer-c  attempts=3 alive=true  backoff=363.257456ms
peer=peer-d  attempts=3 alive=false backoff=841.928214ms
peer=peer-b  attempts=2 alive=true  backoff=106.639967ms
peer=peer-a  attempts=1 alive=true  backoff=0s
```

The probe order (`c, d, b, a`) is not list order -- it is this seed's
shuffle. `peer-a` answers on the first try and pays no backoff at all;
`peer-d` never answers and is marked down only after all three attempts,
having accumulated the largest backoff of the round.

### Tests

Create `gossip_test.go`:

```go
package gossip

import (
	"testing"
	"time"
)

func TestProbeIsDeterministicGivenTheSameSeed(t *testing.T) {
	t.Parallel()

	peers := []string{"peer-a", "peer-b", "peer-c", "peer-d"}
	successAt := map[string]int{"peer-a": 1, "peer-b": 2, "peer-c": 3, "peer-d": 0}
	probe := func(peer string, attempt int) bool {
		want := successAt[peer]
		return want != 0 && attempt >= want
	}

	run := func() []ProbeResult {
		p := New(3, 100*time.Millisecond, 1, 2)
		var got []ProbeResult
		for r := range p.Probe(peers, probe) {
			got = append(got, r)
		}
		return got
	}

	first := run()
	second := run()
	if len(first) != len(second) {
		t.Fatalf("got %d results then %d: same seed must reproduce the same round", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("result[%d] = %+v, then %+v: same seed must be deterministic", i, first[i], second[i])
		}
	}
}

func TestProbeSucceedsOnFirstAttemptHasZeroBackoff(t *testing.T) {
	t.Parallel()

	p := New(3, 50*time.Millisecond, 7, 8)
	alwaysUp := func(string, int) bool { return true }

	for r := range p.Probe([]string{"solo"}, alwaysUp) {
		if !r.Alive || r.Attempt != 1 || r.Backoff != 0 {
			t.Fatalf("got %+v, want alive=true attempt=1 backoff=0", r)
		}
	}
}

func TestProbeExhaustsAttemptsMarksDown(t *testing.T) {
	t.Parallel()

	const maxAttempts = 4
	p := New(maxAttempts, 10*time.Millisecond, 3, 4)
	neverUp := func(string, int) bool { return false }

	for r := range p.Probe([]string{"ghost"}, neverUp) {
		if r.Alive {
			t.Fatalf("got alive=true, want false for a peer that never answers")
		}
		if r.Attempt != maxAttempts {
			t.Fatalf("got Attempt=%d, want %d (all attempts exhausted)", r.Attempt, maxAttempts)
		}
		if r.Backoff <= 0 {
			t.Fatalf("got Backoff=%v, want > 0 after at least one failed attempt", r.Backoff)
		}
	}
}

func TestProbeStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	calls := 0
	countingProbe := func(peer string, attempt int) bool {
		calls++
		return true // succeed immediately so each peer costs exactly one call
	}

	p := New(2, 10*time.Millisecond, 5, 6)
	peers := []string{"p1", "p2", "p3", "p4", "p5"}

	seen := 0
	for range p.Probe(peers, countingProbe) {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("seen = %d, want 2", seen)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2: probing must stop, not run every peer", calls)
	}
}

func TestNewPanicsOnInvalidArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		maxAttempts int
		baseBackoff time.Duration
	}{
		{"zero attempts", 0, time.Millisecond},
		{"zero backoff", 3, 0},
		{"negative backoff", 3, -time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			New(tc.maxAttempts, tc.baseBackoff, 1, 1)
		})
	}
}
```

## Review

The determinism test is not incidental to this exercise -- it is the proof
that seeding `*rand.Rand` explicitly rather than reaching for an unseeded
global source was the right call. The common mistake in a hand-rolled gossip
prober is calling `rand.IntN` or `time.Now().UnixNano()` directly inside the
probe loop: it works, and it is untestable, because every run produces a
different shuffle and a different jitter, and a test asserting on either
becomes either flaky or vacuous. The second thing worth internalizing is why
jitter is *randomized*, not just "backoff, but slightly less": if every
prober in the cluster computed the exact same `baseBackoff*2^(attempt-1)`
with no randomness at all, every prober that started probing a recovering
peer at roughly the same moment would retry at the exact same moment too,
which reproduces the very thundering-herd problem exponential backoff was
supposed to prevent.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [`math/rand/v2` package documentation](https://pkg.go.dev/math/rand/v2)
- [AWS Architecture Blog: Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-feature-flag-targeting-evaluator.md](29-feature-flag-targeting-evaluator.md) | Next: [31-time-series-bucketing-aggregator.md](31-time-series-bucketing-aggregator.md)
