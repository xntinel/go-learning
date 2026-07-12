# Exercise 29: Overload shedding with queue backpressure rejection

**Nivel: Intermedio** — validacion rapida (un test corto).

A shared work queue in front of a fixed pool of workers has a hard capacity:
past that point, admitting more work does not make it run any faster, it
just delays the rejection a caller needs to react to (retry with backoff,
fail over, or surface a `503`) until well after the deadline has already
passed. Admission control enforces this by checking requests against
remaining capacity in priority order — paid tenants before free ones, for
instance — and the instant capacity is gone, shedding everything left
rather than queuing it somewhere it will only ever time out. This module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
admission/                  independent module: example.com/admission
  go.mod                     go 1.24
  admission.go                 Request, Tenant, Gate
  cmd/
    demo/
      main.go                runnable demo: three tenants, capacity runs out mid-scan
  admission_test.go            table test: no tenants, capacity covers everything, zero capacity, a mid-tenant capacity exhaustion, exhaustion exactly on a tenant boundary
```

- Files: `admission.go`, `cmd/demo/main.go`, `admission_test.go`.
- Implement: `Gate(tenants []Tenant, capacity int) (admitted, shed []string)`, admitting requests from tenants in priority order until capacity runs out, then shedding every request not yet admitted in one bulk pass.
- Test: no tenants at all, capacity covering every request, zero capacity shedding everything, capacity running out partway through one tenant's requests, and capacity exhausted exactly on a tenant boundary.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the capacity halt needs a labeled break, not a per-request check

`Gate` walks tenants in priority order in the outer loop, and each tenant's
requests in an inner loop, decrementing `remaining` capacity for every
admission. A version that never used a label at all would still be
*correct*: check `remaining == 0` at the top of each request, admit or shed
accordingly, and let both loops run to completion naturally. But once
capacity hits zero it never recovers within a single `Gate` call — every
request seen after that point is shed, guaranteed, with certainty decided
the instant the last unit of capacity was consumed. Re-deriving that same
conclusion one request at a time, possibly across thousands of pending
requests from tenants further down the priority list, is wasted work on an
outcome that is already fixed.

`break admission`, fired from inside the per-request loop the moment
capacity reaches zero, stops both loops together at the exact request where
admission ended. Everything from that point on — the rest of the current
tenant's requests, and every tenant after it — is shed in a single bulk
pass afterward, without re-checking a capacity that cannot change. The
halted position (which tenant, which request index) is also exactly what a
production admission gate would want to log or export as a metric: not just
"the queue is full," but precisely where the cutoff landed.

Create `admission.go`:

```go
package admission

// Request is one pending unit of work awaiting admission into the shared
// work queue.
type Request struct {
	ID string
}

// Tenant groups the requests awaiting admission for one client, checked in
// priority order (e.g. paid tier before free tier).
type Tenant struct {
	Name     string
	Requests []Request
}

// Gate admits requests from tenants, in priority order, into a shared work
// queue of the given capacity. The instant capacity is exhausted, every
// request not yet admitted -- the rest of the tenant mid-scan, and every
// tenant after it -- is shed in one pass rather than admitted, and the exact
// point admission stopped is remembered for diagnostics. A queue that is
// already full cannot promise to eventually run more work handed to it;
// continuing to walk each remaining request individually just burns CPU
// confirming, one at a time, a rejection that is already certain.
func Gate(tenants []Tenant, capacity int) (admitted, shed []string) {
	remaining := capacity
	haltTenant, haltRequest := -1, -1

admission:
	for i, t := range tenants {
		for j, r := range t.Requests {
			if remaining == 0 {
				// Capacity ran out at this exact request. A bare break here
				// would leave only the per-request loop for tenant t, and
				// the outer loop would move on to the NEXT tenant and
				// re-derive the same "queue is full" conclusion one request
				// at a time -- correct, but wasteful for a queue backed by
				// thousands of pending requests across many tenants.
				// break admission stops both loops here, in one place, so
				// the bulk-shed pass below can run once instead of
				// re-checking a capacity that is not going to change.
				haltTenant, haltRequest = i, j
				break admission
			}
			admitted = append(admitted, r.ID)
			remaining--
		}
	}

	if haltTenant >= 0 {
		shed = append(shed, tenants[haltTenant].Requests[haltRequest].ID)
		for _, r := range tenants[haltTenant].Requests[haltRequest+1:] {
			shed = append(shed, r.ID)
		}
		for _, t := range tenants[haltTenant+1:] {
			for _, r := range t.Requests {
				shed = append(shed, r.ID)
			}
		}
	}
	return admitted, shed
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/admission"
)

func main() {
	tenants := []admission.Tenant{
		{Name: "paid", Requests: []admission.Request{{ID: "p1"}, {ID: "p2"}, {ID: "p3"}}},
		{Name: "free", Requests: []admission.Request{{ID: "f1"}, {ID: "f2"}, {ID: "f3"}}},
		{Name: "batch", Requests: []admission.Request{{ID: "b1"}, {ID: "b2"}}},
	}

	admitted, shed := admission.Gate(tenants, 4)
	fmt.Println("admitted:", admitted)
	fmt.Println("shed:", shed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
admitted: [p1 p2 p3 f1]
shed: [f2 f3 b1 b2]
```

`paid` gets all three of its requests through; `free` gets exactly one
before the shared capacity of four runs out. `free`'s remaining two
requests are shed, and so is every request belonging to `batch`, even
though `batch` was never even reached by the per-request scan.

### Tests

`TestGate` covers no tenants at all, capacity generous enough to admit
everything, zero capacity shedding every request in priority order, capacity
running out partway through one tenant (the core case, matching the demo),
and the boundary where capacity is exhausted exactly on a tenant's last
request rather than mid-tenant.

Create `admission_test.go`:

```go
package admission

import (
	"slices"
	"testing"
)

func req(ids ...string) []Request {
	reqs := make([]Request, len(ids))
	for i, id := range ids {
		reqs[i] = Request{ID: id}
	}
	return reqs
}

func TestGate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		tenants   []Tenant
		capacity  int
		wantAdmit []string
		wantShed  []string
	}{
		"no tenants": {
			tenants:   nil,
			capacity:  10,
			wantAdmit: nil,
			wantShed:  nil,
		},
		"capacity covers every request": {
			tenants: []Tenant{
				{Name: "paid", Requests: req("p1", "p2")},
				{Name: "free", Requests: req("f1")},
			},
			capacity:  10,
			wantAdmit: []string{"p1", "p2", "f1"},
			wantShed:  nil,
		},
		"zero capacity sheds everything, in priority order": {
			tenants: []Tenant{
				{Name: "paid", Requests: req("p1", "p2")},
				{Name: "free", Requests: req("f1")},
			},
			capacity:  0,
			wantAdmit: nil,
			wantShed:  []string{"p1", "p2", "f1"},
		},
		"capacity runs out mid-tenant, shedding the rest of it and every tenant after": {
			tenants: []Tenant{
				{Name: "paid", Requests: req("p1", "p2", "p3")},
				{Name: "free", Requests: req("f1", "f2", "f3")},
				{Name: "batch", Requests: req("b1", "b2")},
			},
			capacity:  4,
			wantAdmit: []string{"p1", "p2", "p3", "f1"},
			wantShed:  []string{"f2", "f3", "b1", "b2"},
		},
		"capacity exhausted exactly on a tenant boundary": {
			tenants: []Tenant{
				{Name: "paid", Requests: req("p1", "p2")},
				{Name: "free", Requests: req("f1")},
			},
			capacity:  2,
			wantAdmit: []string{"p1", "p2"},
			wantShed:  []string{"f1"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			admitted, shed := Gate(tc.tenants, tc.capacity)
			if !slices.Equal(admitted, tc.wantAdmit) {
				t.Fatalf("admitted = %v, want %v", admitted, tc.wantAdmit)
			}
			if !slices.Equal(shed, tc.wantShed) {
				t.Fatalf("shed = %v, want %v", shed, tc.wantShed)
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

`Gate` is correct when the set of admitted requests is exactly a prefix of
the priority-ordered stream up to the capacity limit, and every request
after that point — regardless of which tenant it belongs to — is shed. The
"capacity runs out mid-tenant" test is the one to study: `free`'s `f1` is
admitted but `f2` and `f3` are shed even though they belong to the same
tenant, and `batch`'s requests are shed without the scan ever reaching them
individually. The design choice worth defending in review is the labeled
break itself: a per-request capacity check with no label would produce the
identical `admitted`/`shed` sets, so the label is not fixing a correctness
bug here — it is what lets the halt point be captured once and the bulk
shed happen in a single pass, instead of re-confirming a foregone conclusion
for every request that follows.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [Google SRE Book: Handling Overload](https://sre.google/sre-book/handling-overload/) — the load-shedding strategy this gate implements.
- [AWS Builders' Library: Using load shedding to avoid overload](https://aws.amazon.com/builders-library/using-load-shedding-to-avoid-overload/) — priority-based admission control in a real system.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-bloom-filter-membership-dedup.md](28-bloom-filter-membership-dedup.md) | Next: [30-request-coalescing-singleflight.md](30-request-coalescing-singleflight.md)
