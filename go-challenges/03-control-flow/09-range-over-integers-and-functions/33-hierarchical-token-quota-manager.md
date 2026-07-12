# Exercise 33: Hierarchical Token Quota Manager — Multi-Level Org/User/Endpoint Rate Limits

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Enforcing "no organization does more than 1,000 requests/minute, no single
user more than 100, no single endpoint more than 20" only works if a request
that fails at the endpoint level never quietly drains a token from the
organization's budget on its way to being rejected -- a partial consumption
on denial slowly leaks capacity out of the levels that "succeeded" for
requests that ultimately never happened, and after enough leaked tokens the
organization's supposed 1,000/minute ceiling silently becomes something
less, with no error and no obvious cause. This exercise builds a quota tree
where a request is admitted or denied as one atomic unit across every level
that applies to it. This exercise is an independent module with its own
`go mod init`.

## What you'll build

```text
quota/                     independent module: example.com/hierarchical-token-quota-manager
  go.mod                    module example.com/hierarchical-token-quota-manager
  quota.go                  Manager, New, SetOrgLimit, SetUserLimit, SetEndpointLimit, Request, Admit, AdmitAll
  cmd/
    demo/
      main.go               runnable demo: 6 requests across org/user/endpoint limits
  quota_test.go              three-level enforcement, atomic denial, AdmitAll ordering, concurrency bound, panic
```

Implement: `New() *Manager`, `SetOrgLimit`/`SetUserLimit`/`SetEndpointLimit(...,  capacity int)` to configure each level independently, `(*Manager) Admit(req Request) bool` checking and consuming a token from every configured applicable level as one atomic unit, and `(*Manager) AdmitAll(requests iter.Seq[Request]) iter.Seq[Request]` filtering to admitted requests.
Test: a request is denied exactly at the level whose limit is exhausted, and levels without a configured limit are treated as unlimited; a denied request leaves every level's token count untouched, provable by exhausting a narrow endpoint limit and confirming the wider org/user budget is still fully available; `AdmitAll` preserves order and filters denied requests; 50 goroutines racing 5 requests each against a 20-token org budget admit exactly 20, never more; a negative capacity panics.
Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

`Manager` guards every level's token bucket with a single `sync.Mutex`
instead of one lock per tree node. That is a deliberate trade, not a
shortcut: since `Admit` must check up to three levels and then consume from
all of them as a single atomic decision, per-node locking would require a
fixed lock-acquisition order across org, user, and endpoint on every single
call just to avoid deadlocking against a concurrent call that touches the
same three levels in a different order -- for zero additional concurrency,
because the decision has to serialize on something regardless. One coarse
lock makes the "all or nothing" property trivially true: `Admit` holds the
lock across both the check phase and the consume phase, so there is no
window where another goroutine could see a level as available, get admitted
based on stale headroom, and push the real token count negative. A level
that has no configured limit is treated as unlimited by simply never
appearing in the internal `buckets` map for that key -- `Admit` skips
anything it does not find, rather than treating a missing configuration as
a hard zero, which would make every request to an unconfigured endpoint
fail closed instead of the intended fail open.

Create `quota.go`:

```go
package quota

import (
	"iter"
	"sync"
)

// bucket is one level's token allowance: capacity is the configured limit,
// tokens is what remains.
type bucket struct {
	capacity, tokens int
}

// Request identifies which org, user, and endpoint a single call belongs
// to. Endpoint is only consulted if an endpoint-level limit was configured
// for this exact (Org, User, Endpoint) triple.
type Request struct {
	Org, User, Endpoint string
}

// Manager enforces token quotas across three nested levels: organization,
// user within an organization, and endpoint within a user. A single
// sync.Mutex guards every bucket rather than one lock per tree node: since
// admitting a request must check and consume tokens at up to three levels
// as a single atomic unit, per-node locking would need a fixed lock
// ordering across org/user/endpoint just to avoid deadlock, for no
// throughput benefit -- the whole decision already has to serialize on
// something, so one coarse lock is both simpler and exactly as correct.
type Manager struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// New creates an empty Manager. No level has a configured limit until one
// of the Set*Limit methods is called for it.
func New() *Manager {
	return &Manager{buckets: make(map[string]*bucket)}
}

func (m *Manager) setLimit(key string, capacity int) {
	if capacity < 0 {
		panic("quota: capacity must be >= 0")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[key] = &bucket{capacity: capacity, tokens: capacity}
}

// SetOrgLimit configures the token capacity for an entire organization.
func (m *Manager) SetOrgLimit(org string, capacity int) {
	m.setLimit(org, capacity)
}

// SetUserLimit configures the token capacity for one user within an
// organization, independent of that organization's own limit.
func (m *Manager) SetUserLimit(org, user string, capacity int) {
	m.setLimit(org+"|"+user, capacity)
}

// SetEndpointLimit configures the token capacity for one endpoint scoped to
// one user within an organization.
func (m *Manager) SetEndpointLimit(org, user, endpoint string, capacity int) {
	m.setLimit(org+"|"+user+"|"+endpoint, capacity)
}

// Admit atomically checks and consumes one token from every configured
// level that applies to req -- org, org/user, and org/user/endpoint. A
// level with no configured limit is treated as unlimited: it is neither
// checked nor consumed. If any configured level has no tokens left, Admit
// denies the request and consumes nothing at all, at any level -- not even
// at levels that did have room -- because partially consuming quota for a
// request that is ultimately rejected would silently leak capacity from
// the levels that "succeeded," making the total admitted load exceed what
// the caller believes each level's limit permits.
func (m *Manager) Admit(req Request) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := [3]string{
		req.Org,
		req.Org + "|" + req.User,
		req.Org + "|" + req.User + "|" + req.Endpoint,
	}

	var applicable []*bucket
	for _, k := range keys {
		if b, ok := m.buckets[k]; ok {
			if b.tokens <= 0 {
				return false
			}
			applicable = append(applicable, b)
		}
	}
	for _, b := range applicable {
		b.tokens--
	}
	return true
}

// AdmitAll filters requests down to only those Admit accepts, preserving
// order, and consuming quota as a side effect of iteration -- exactly as if
// the caller had called Admit on each request itself.
func (m *Manager) AdmitAll(requests iter.Seq[Request]) iter.Seq[Request] {
	return func(yield func(Request) bool) {
		for req := range requests {
			if m.Admit(req) {
				if !yield(req) {
					return
				}
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

	"example.com/hierarchical-token-quota-manager"
)

func main() {
	m := quota.New()
	m.SetOrgLimit("acme", 5)
	m.SetUserLimit("acme", "alice", 3)
	m.SetEndpointLimit("acme", "alice", "search", 2)

	requests := []quota.Request{
		{Org: "acme", User: "alice", Endpoint: "search"},
		{Org: "acme", User: "alice", Endpoint: "search"},
		{Org: "acme", User: "alice", Endpoint: "search"},  // endpoint exhausted
		{Org: "acme", User: "alice", Endpoint: "billing"}, // no endpoint limit set
		{Org: "acme", User: "alice", Endpoint: "billing"}, // user exhausted
		{Org: "acme", User: "bob", Endpoint: "search"},    // no user/endpoint limit for bob
	}

	for _, req := range requests {
		fmt.Printf("org=%s user=%-6s endpoint=%-8s admitted=%v\n",
			req.Org, req.User, req.Endpoint, m.Admit(req))
	}
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
org=acme user=alice  endpoint=search   admitted=true
org=acme user=alice  endpoint=search   admitted=true
org=acme user=alice  endpoint=search   admitted=false
org=acme user=alice  endpoint=billing  admitted=true
org=acme user=alice  endpoint=billing  admitted=false
org=acme user=bob    endpoint=search   admitted=true
```

The third request is denied purely by the `search` endpoint's 2-token limit
even though `alice` and `acme` both still have headroom; the fourth request
to `billing` (no endpoint limit configured) succeeds and consumes `alice`'s
last user-level token, which is why the fifth request is denied at the user
level; `bob`'s request succeeds because neither a user nor an endpoint limit
was ever configured for him, leaving only the shared org budget to check.

### Tests

Create `quota_test.go`:

```go
package quota

import (
	"iter"
	"sync"
	"sync/atomic"
	"testing"
)

func reqSeq(reqs []Request) iter.Seq[Request] {
	return func(yield func(Request) bool) {
		for _, r := range reqs {
			if !yield(r) {
				return
			}
		}
	}
}

func TestAdmitEnforcesAllThreeLevels(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetOrgLimit("acme", 5)
	m.SetUserLimit("acme", "alice", 3)
	m.SetEndpointLimit("acme", "alice", "search", 2)

	reqs := []Request{
		{Org: "acme", User: "alice", Endpoint: "search"},
		{Org: "acme", User: "alice", Endpoint: "search"},
		{Org: "acme", User: "alice", Endpoint: "search"},  // endpoint exhausted
		{Org: "acme", User: "alice", Endpoint: "billing"}, // no endpoint limit
		{Org: "acme", User: "alice", Endpoint: "billing"}, // user exhausted
		{Org: "acme", User: "bob", Endpoint: "search"},    // unlimited user/endpoint
	}
	want := []bool{true, true, false, true, false, true}

	for i, req := range reqs {
		got := m.Admit(req)
		if got != want[i] {
			t.Fatalf("Admit(%+v) at index %d = %v, want %v", req, i, got, want[i])
		}
	}
}

func TestAdmitDeniesAsAUnitWithoutPartialConsumption(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetOrgLimit("acme", 10)
	m.SetUserLimit("acme", "alice", 10)
	m.SetEndpointLimit("acme", "alice", "search", 1)

	req := Request{Org: "acme", User: "alice", Endpoint: "search"}
	if !m.Admit(req) {
		t.Fatal("first request should be admitted (endpoint has 1 token)")
	}
	// Endpoint is now exhausted; org and user still have 9 tokens each.
	if m.Admit(req) {
		t.Fatal("second request should be denied: endpoint exhausted")
	}

	// A different endpoint (no per-endpoint limit) must still be admitted,
	// proving org and user tokens were never touched by the denied request.
	other := Request{Org: "acme", User: "alice", Endpoint: "other"}
	for i := 0; i < 9; i++ {
		if !m.Admit(other) {
			t.Fatalf("request %d to the unlimited endpoint should be admitted (org/user still had headroom)", i)
		}
	}
	if m.Admit(other) {
		t.Fatal("10th request should be denied: org and user tokens are now exhausted")
	}
}

func TestAdmitAllPreservesOrderAndFiltersDenied(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetOrgLimit("acme", 2)

	reqs := []Request{
		{Org: "acme", User: "a", Endpoint: "e"},
		{Org: "acme", User: "b", Endpoint: "e"},
		{Org: "acme", User: "c", Endpoint: "e"}, // org exhausted by here
	}

	var got []string
	for r := range m.AdmitAll(reqSeq(reqs)) {
		got = append(got, r.User)
	}
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestAdmitNeverExceedsCapacityConcurrently(t *testing.T) {
	t.Parallel()

	const capacity = 20
	const goroutines = 50
	const perGoroutine = 5 // 250 attempts total against a 20-token budget

	m := New()
	m.SetOrgLimit("acme", capacity)

	var admitted int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if m.Admit(Request{Org: "acme", User: "u", Endpoint: "e"}) {
					atomic.AddInt64(&admitted, 1)
				}
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&admitted); got != capacity {
		t.Fatalf("admitted = %d, want exactly %d (the org's full but only its full capacity)", got, capacity)
	}
}

func TestSetLimitPanicsOnNegativeCapacity(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for a negative capacity")
		}
	}()
	New().SetOrgLimit("acme", -1)
}
```

## Review

`TestAdmitDeniesAsAUnitWithoutPartialConsumption` is the test that actually
defends this design's reason for existing: it proves that exhausting a
narrow endpoint limit leaves the wider org and user budgets completely
intact, by immediately spending all nine of the remaining org/user tokens
through a different, unlimited endpoint. If `Admit` instead decremented
each level as it checked it -- consuming the org and user tokens before
discovering the endpoint was out -- that same sequence would show fewer
than nine successful follow-up requests, silently confirming the leak.
`TestAdmitNeverExceedsCapacityConcurrently` defends the other half of the
contract: with 250 concurrent attempts racing a 20-token budget, the count
admitted must land on exactly 20, neither more (which would mean the
check-then-consume sequence has a race window) nor fewer (which would mean
tokens are being lost, not merely denied).

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Stripe: rate limiters](https://stripe.com/blog/rate-limiters)
- [The Go Memory Model](https://go.dev/ref/mem)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-request-coalescing-singleflight.md](32-request-coalescing-singleflight.md) | Next: [34-write-ahead-log-compaction-iterator.md](34-write-ahead-log-compaction-iterator.md)
