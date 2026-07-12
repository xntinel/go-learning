# Exercise 16: Verify read quorum replicas for consistency

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed storage system serves a read by fetching from a quorum of
replicas and expects them to agree. Comparing every byte of every replica
against every other replica is wasted work — the moment any replica diverges
from the first one fetched, the read is already inconsistent and needs
repair, regardless of what the rest would have shown. This module is fully
self-contained: its own `go mod init`, all code inline, its own demo and
tests.

## What you'll build

```text
quorum/                     independent module: example.com/quorum
  go.mod                     go 1.24
  quorum.go                  Replica, CheckQuorum
  cmd/
    demo/
      main.go                runnable demo: one consistent set, one diverged set
  quorum_test.go              table test: single replica, all match, byte mismatch, length mismatch, first-wins
```

- Files: `quorum.go`, `cmd/demo/main.go`, `quorum_test.go`.
- Implement: `CheckQuorum(replicas []Replica) (consistent bool, mismatchID string, mismatchIndex int)`, comparing every replica after the first against the first and stopping at the first divergence.
- Test: a single replica, an identical set, a byte-level mismatch, a length mismatch, and proof that only the first divergent replica is ever reported.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the byte comparison needs a labeled break

`CheckQuorum` has two loops: the outer one walks the replicas after the
first, and the inner one walks the bytes of each replica against the
reference. The instant a byte differs, the entire check is over — there is
no reason to keep comparing the rest of this replica, and no reason to look
at any replica after it. A bare `break` inside the byte-comparison loop would
only leave that inner loop; the outer loop would then move on to the *next*
replica and keep comparing, silently continuing work that no longer matters
and potentially overwriting the reported mismatch with a different one. The
labeled `break replicas` leaves both loops in the same statement, so the
first divergence found is the one reported, full stop. A length mismatch is
checked before the byte loop even starts, since comparing bytes of two
different-length slices index-out-of-range risks are exactly the kind of bug
this ordering avoids.

Create `quorum.go`:

```go
package quorum

// Replica is one member of a read quorum, holding the bytes it returned.
type Replica struct {
	ID   string
	Data []byte
}

// CheckQuorum compares every replica after the first against the first
// replica's data, byte by byte. The instant any byte differs (or the
// lengths differ), the quorum is inconsistent and the WHOLE scan stops
// immediately: there is no value in comparing the remaining replicas once
// one has already proven a mismatch. The byte-by-byte comparison is a loop
// nested inside the per-replica loop, so reporting the mismatch and leaving
// both loops in one motion requires a labeled break on the outer loop.
func CheckQuorum(replicas []Replica) (consistent bool, mismatchID string, mismatchIndex int) {
	if len(replicas) < 2 {
		return true, "", -1
	}
	reference := replicas[0].Data
	mismatchIndex = -1

replicas:
	for _, r := range replicas[1:] {
		if len(r.Data) != len(reference) {
			mismatchID = r.ID
			mismatchIndex = min(len(r.Data), len(reference))
			break replicas
		}
		for i, b := range reference {
			if r.Data[i] != b {
				mismatchID = r.ID
				mismatchIndex = i
				break replicas
			}
		}
	}

	return mismatchID == "", mismatchID, mismatchIndex
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/quorum"
)

func main() {
	consistentSet := []quorum.Replica{
		{ID: "r1", Data: []byte("order-42:paid")},
		{ID: "r2", Data: []byte("order-42:paid")},
		{ID: "r3", Data: []byte("order-42:paid")},
	}
	ok, mismatchID, idx := quorum.CheckQuorum(consistentSet)
	fmt.Println("consistent set:", ok, mismatchID, idx)

	divergedSet := []quorum.Replica{
		{ID: "r1", Data: []byte("order-42:paid")},
		{ID: "r2", Data: []byte("order-42:paid")},
		{ID: "r3", Data: []byte("order-42:refunded")},
	}
	ok, mismatchID, idx = quorum.CheckQuorum(divergedSet)
	fmt.Println("diverged set:  ", ok, mismatchID, idx)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
consistent set: true  -1
diverged set:   false r3 13
```

The diverged set's third replica has a different length than the first two
(`"order-42:refunded"` versus `"order-42:paid"`), so the mismatch is caught
by the length check before a single byte is compared, and the reported index
is where the shorter of the two data slices ends.

### Tests

`TestCheckQuorum` covers a single replica (trivially consistent), a fully
matching set, a single differing byte, a length mismatch, and a set where a
second divergent replica exists but is never reported because the first one
already stopped the scan.

Create `quorum_test.go`:

```go
package quorum

import "testing"

func TestCheckQuorum(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		replicas       []Replica
		wantConsistent bool
		wantMismatchID string
		wantIndex      int
	}{
		"single replica is trivially consistent": {
			replicas:       []Replica{{ID: "r1", Data: []byte("v1")}},
			wantConsistent: true,
			wantIndex:      -1,
		},
		"all replicas identical": {
			replicas: []Replica{
				{ID: "r1", Data: []byte("v1")},
				{ID: "r2", Data: []byte("v1")},
				{ID: "r3", Data: []byte("v1")},
			},
			wantConsistent: true,
			wantIndex:      -1,
		},
		"a later replica byte-diverges": {
			replicas: []Replica{
				{ID: "r1", Data: []byte("abc")},
				{ID: "r2", Data: []byte("abc")},
				{ID: "r3", Data: []byte("abd")},
			},
			wantConsistent: false,
			wantMismatchID: "r3",
			wantIndex:      2,
		},
		"a length mismatch is caught before any byte comparison": {
			replicas: []Replica{
				{ID: "r1", Data: []byte("abc")},
				{ID: "r2", Data: []byte("abcd")},
			},
			wantConsistent: false,
			wantMismatchID: "r2",
			wantIndex:      3,
		},
		"the first divergent replica wins, later ones are never inspected": {
			replicas: []Replica{
				{ID: "r1", Data: []byte("abc")},
				{ID: "r2", Data: []byte("xbc")},
				{ID: "r3", Data: []byte("zzz")},
			},
			wantConsistent: false,
			wantMismatchID: "r2",
			wantIndex:      0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			consistent, mismatchID, idx := CheckQuorum(tc.replicas)
			if consistent != tc.wantConsistent || mismatchID != tc.wantMismatchID || idx != tc.wantIndex {
				t.Fatalf("CheckQuorum = (%v, %q, %d), want (%v, %q, %d)",
					consistent, mismatchID, idx, tc.wantConsistent, tc.wantMismatchID, tc.wantIndex)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The checker is correct when it reports the *first* divergent replica and
touches nothing after it — the last test case proves this by planting a
second, different divergence in `r3` that would change the reported index if
the scan kept going. The bug this exercise guards against is a bare `break`
inside the byte-comparison loop: it would leave only that loop, and the outer
loop would proceed to compare the next replica, potentially replacing a real
mismatch with a later, unrelated one, or wasting a full comparison pass after
the answer was already known.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [bytes.Equal](https://pkg.go.dev/bytes#Equal) — the standard library's own byte-slice comparison, for contrast with a manual scan that needs the mismatch position.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-binary-protocol-frame-parser.md](15-binary-protocol-frame-parser.md) | Next: [17-outbox-event-batcher.md](17-outbox-event-batcher.md)
