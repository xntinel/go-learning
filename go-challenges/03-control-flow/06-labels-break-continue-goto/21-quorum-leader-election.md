# Exercise 21: Establish a leader across three zones by finding first quorum

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed system elects a leader by checking geographic zones in
priority order — the operator's declared preference, or simple proximity to
most traffic. A zone is a viable candidate only once a quorum (a strict
majority) of its replicas are healthy, and each replica's health is itself
determined by a retried probe sequence rather than a single ping. The first
zone to reach quorum is promoted; lower-priority zones are never even
inspected once that happens. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
election/                   independent module: example.com/election
  go.mod                     go 1.24
  election.go                Zone, Replica, ElectLeader
  cmd/
    demo/
      main.go                runnable demo: three zones, quorum reached in the second
  election_test.go            table test: no zones, zero-replica zone, first-quorum-wins, no quorum anywhere, late-probe health, exact-majority boundary, plus a concurrent-callers race check
```

- Files: `election.go`, `cmd/demo/main.go`, `election_test.go`.
- Implement: `ElectLeader(zones []Zone) (leader string, elected bool)`, promoting the first zone (in the given order) whose healthy-replica count is a strict majority of its total replicas.
- Test: no zones, a zone with zero replicas, the first qualifying zone winning over a later, also-qualifying one, no zone reaching quorum, a replica that only proves healthy on its last probe, and the boundary where an even replica count needs *more* than half. A concurrency test confirms `ElectLeader` is safe to call from many goroutines against the same read-only input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the label has to reach past a whole loop, from the innermost one

`ElectLeader` is three loops deep: zones, then a zone's replicas, then a
replica's probe sequence. Two decisions happen from the innermost probe
loop, and both need a label to reach further than one level up:

- The moment a probe succeeds, the replica is healthy and its remaining
  probes are irrelevant — but a bare `break` there would only leave the
  probe loop, landing back at the top of the *same* replica's loop body with
  nothing left to check, so `continue replicas` is used instead to state the
  intent directly: move to the next replica.
- The moment the running healthy count crosses quorum, the whole election is
  decided. A bare `break` would only leave the probe loop; even a `break`
  labeled on `replicas` would only stop scanning this zone's remaining
  replicas, but the OUTER zones loop would then move on and evaluate the
  next zone anyway — wasted work at best, and at worst a bug if a caller
  ever changed the code to prefer, say, the zone with the most healthy
  replicas instead of the first one to qualify. `break zones`, fired from
  two loops below the loop it names, is what actually stops the whole
  election the instant a leader is found.

Quorum is computed as `healthy*2 > total` rather than `healthy > total/2` to
avoid integer-division rounding hiding the boundary: for four replicas,
`2*2 > 4` is false, correctly rejecting exactly-half as a majority, which
`2 > 4/2` (also false) gets right only by chance — the multiplication form
stays correct however the replica count is composed.

Create `election.go`:

```go
package election

// Zone is one geographic zone with its replicas, checked in priority order.
type Zone struct {
	Name     string
	Replicas []Replica
}

// Replica is checked via a sequence of health probes (e.g. retried pings).
// It counts as healthy the moment any probe in its sequence succeeds.
type Replica struct {
	ID     string
	Probes []bool
}

// ElectLeader checks zones in priority order (the order given). A zone
// becomes the leader the moment a QUORUM (a strict majority) of its
// replicas are healthy. Each replica's health is itself determined by a
// probe sequence: the moment one probe succeeds the replica is healthy and
// its remaining probes are skipped with a labeled continue on the REPLICAS
// loop. The moment a zone reaches quorum, the ENTIRE election stops with a
// labeled break on the ZONES loop — fired from inside the innermost probe
// loop, two levels below the loop it names — so no lower-priority zone is
// ever inspected once a leader is found.
func ElectLeader(zones []Zone) (leader string, elected bool) {
zones:
	for _, z := range zones {
		healthy := 0
	replicas:
		for _, r := range z.Replicas {
			for _, ok := range r.Probes {
				if !ok {
					continue
				}
				healthy++
				if healthy*2 > len(z.Replicas) {
					leader = z.Name
					elected = true
					break zones
				}
				continue replicas
			}
		}
	}
	return leader, elected
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/election"
)

func main() {
	zones := []election.Zone{
		{
			Name: "us-east",
			Replicas: []election.Replica{
				{ID: "r1", Probes: []bool{false, false}},
				{ID: "r2", Probes: []bool{false}},
				{ID: "r3", Probes: []bool{true}},
			},
		},
		{
			Name: "us-west",
			Replicas: []election.Replica{
				{ID: "r1", Probes: []bool{true}},
				{ID: "r2", Probes: []bool{false, true}},
				{ID: "r3", Probes: []bool{true}},
			},
		},
		{
			Name: "eu-central",
			Replicas: []election.Replica{
				{ID: "r1", Probes: []bool{true}},
				{ID: "r2", Probes: []bool{true}},
				{ID: "r3", Probes: []bool{true}},
			},
		},
	}

	leader, elected := election.ElectLeader(zones)
	fmt.Println("leader:", leader)
	fmt.Println("elected:", elected)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leader: us-west
elected: true
```

`us-east` only ever gets one healthy replica out of three — not a majority —
so the scan moves on. `us-west` reaches two healthy replicas out of three,
a strict majority, and is promoted immediately. `eu-central`, where every
replica is healthy, is never even inspected: it would also have qualified,
but priority order already picked a winner.

### Tests

`TestElectLeader` is a table covering no zones, a zone with zero replicas,
the priority-order guarantee (first qualifying zone wins over a later one
that would also qualify), no zone reaching quorum at all, a replica that
only proves healthy on its very last probe, and the even-replica-count
boundary where exactly half is not enough. `TestElectLeaderConcurrentCallers`
runs `ElectLeader` from fifty goroutines against the same zones slice to
confirm the read-only scan introduces no data race.

Create `election_test.go`:

```go
package election

import (
	"sync"
	"testing"
)

func TestElectLeader(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		zones      []Zone
		wantLeader string
		wantElect  bool
	}{
		"no zones at all": {
			zones:      nil,
			wantLeader: "",
			wantElect:  false,
		},
		"a zone with zero replicas can never reach quorum": {
			zones: []Zone{
				{Name: "empty-zone", Replicas: nil},
			},
			wantLeader: "",
			wantElect:  false,
		},
		"first zone reaching quorum wins over a later, also-qualifying zone": {
			zones: []Zone{
				{
					Name: "us-east",
					Replicas: []Replica{
						{ID: "r1", Probes: []bool{false}},
						{ID: "r2", Probes: []bool{true}},
						{ID: "r3", Probes: []bool{true}},
					},
				},
				{
					Name: "us-west",
					Replicas: []Replica{
						{ID: "r1", Probes: []bool{true}},
						{ID: "r2", Probes: []bool{true}},
						{ID: "r3", Probes: []bool{true}},
					},
				},
			},
			wantLeader: "us-east",
			wantElect:  true,
		},
		"no zone reaches quorum": {
			zones: []Zone{
				{
					Name: "us-east",
					Replicas: []Replica{
						{ID: "r1", Probes: []bool{false}},
						{ID: "r2", Probes: []bool{false, false}},
						{ID: "r3", Probes: []bool{true}},
					},
				},
			},
			wantLeader: "",
			wantElect:  false,
		},
		"a replica healthy on its last probe still counts toward quorum": {
			zones: []Zone{
				{
					Name: "us-east",
					Replicas: []Replica{
						{ID: "r1", Probes: []bool{false, false, true}},
						{ID: "r2", Probes: []bool{true}},
						{ID: "r3", Probes: []bool{false}},
					},
				},
			},
			wantLeader: "us-east",
			wantElect:  true,
		},
		"exact majority for an even replica count requires more than half": {
			zones: []Zone{
				{
					Name: "four-way",
					Replicas: []Replica{
						{ID: "r1", Probes: []bool{true}},
						{ID: "r2", Probes: []bool{true}},
						{ID: "r3", Probes: []bool{false}},
						{ID: "r4", Probes: []bool{false}},
					},
				},
			},
			wantLeader: "",
			wantElect:  false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			leader, elected := ElectLeader(tc.zones)
			if leader != tc.wantLeader || elected != tc.wantElect {
				t.Fatalf("ElectLeader = (%q, %v), want (%q, %v)", leader, elected, tc.wantLeader, tc.wantElect)
			}
		})
	}
}

// TestElectLeaderConcurrentCallers checks that ElectLeader, which only reads
// its input, is safe to call concurrently from many goroutines sharing the
// same zones slice -- run with -race to confirm no data race is introduced.
func TestElectLeaderConcurrentCallers(t *testing.T) {
	zones := []Zone{
		{
			Name: "us-east",
			Replicas: []Replica{
				{ID: "r1", Probes: []bool{false}},
				{ID: "r2", Probes: []bool{true}},
				{ID: "r3", Probes: []bool{true}},
			},
		},
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leader, elected := ElectLeader(zones)
			if leader != "us-east" || !elected {
				t.Errorf("ElectLeader = (%q, %v), want (%q, %v)", leader, elected, "us-east", true)
			}
		}()
	}
	wg.Wait()
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The election is correct when it returns the FIRST zone to reach quorum, in
priority order, and never inspects a lower-priority zone once that happens
— the "first zone reaching quorum wins" test is the one to study, since its
second zone has a unanimous, unambiguously-better quorum and the code must
still ignore it. The bug this exercise targets is a `break` that only
reaches the `replicas` label instead of `zones`: it would correctly stop
checking the winning zone's remaining replicas, but the outer zones loop
would still run to completion, silently overwriting `leader` if the code
were ever restructured to keep scanning. The exact-majority test pins the
quorum arithmetic itself: `healthy*2 > total`, not `healthy >= total/2`,
which would wrongly accept exactly half as sufficient for an even replica
count. The concurrent-callers test confirms the function's real production
use — called from many request-handling goroutines against the same
zone-health snapshot — never touches shared mutable state.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once, regardless of nesting depth.
- [Raft consensus paper, §5.2 Leader election](https://raft.github.io/raft.pdf) — the quorum-based election shape this function models.
- [The Go Memory Model](https://go.dev/ref/mem) — why a function that only reads shared data needs no synchronization to be race-free.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-failure-rate-circuit-breaker.md](20-failure-rate-circuit-breaker.md) | Next: [22-connection-pool-lifecycle-rebalance.md](22-connection-pool-lifecycle-rebalance.md)
