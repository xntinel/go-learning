# Exercise 23: Broadcast Cache Invalidations Across Replica Pools

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache coherency system fans one invalidation out to every replica in a
pool, concurrently, so that a stale value is evicted everywhere at roughly
the same time. Three invalidation kinds exist — set a key to a new value,
delete a key outright, or purge every key under a prefix — and each maps to
a different method on the `Replica` interface. Two production realities
complicate what would otherwise be a simple fan-out: a message bus delivers
at-least-once, so the same invalidation can arrive twice, and any individual
replica can fail transiently, so one flaky node must not sink the whole
broadcast nor silently lose the invalidation.

## What you'll build

```text
cache-invalidation-broadcaster/  independent module: example.com/cache-invalidation-broadcaster
  go.mod                          go 1.24
  broadcaster.go                  (*Broadcaster).Broadcast(inv any, replicas []Replica) error
  cmd/
    demo/
      main.go                     broadcasts three invalidation kinds to two replicas
  broadcaster_test.go              table of cases plus dedup, retry, and concurrency tests
```

- Files: `broadcaster.go`, `cmd/demo/main.go`, `broadcaster_test.go`.
- Implement: `(*Broadcaster).Broadcast(inv any, replicas []Replica) error`,
  type-switching on `UpdateInvalidation`, `DeleteInvalidation`, and
  `PurgePrefixInvalidation` in both a `dedupKey` function and an `apply`
  function.
- Test: each invalidation kind dispatching to its matching `Replica`
  method, a repeat delivery being a no-op, a transient failure being masked
  by one retry, a replica that fails twice returning a joined error, an
  unsupported invalidation kind, and many goroutines broadcasting
  concurrently to prove the dedup set is race-free.

Set up the module:

```bash
mkdir -p ~/go-exercises/cache-invalidation-broadcaster/cmd/demo
cd ~/go-exercises/cache-invalidation-broadcaster
go mod init example.com/cache-invalidation-broadcaster
go mod edit -go=1.24
```

The type switch appears twice, in two different roles, and that split is
deliberate rather than duplication: `dedupKey` derives an invalidation's
*identity* (so a redelivered message collapses to the same cache entry
instead of re-running the broadcast), while `apply` derives its *effect* on
one replica. Keeping them separate means the dedup key can be computed once,
before any goroutines are spawned, while `apply` is what actually runs
concurrently per replica — conflating the two into one switch would either
recompute the identity per replica (wasteful and a source of subtle
inconsistency if the two computations ever drifted) or fan out before
checking whether this invalidation was already handled. `errors.Join`
(Go 1.20+) is what lets a permanently failing replica's error surface
without hiding a different replica's independent failure behind it — two
distinct problems on two distinct replicas both need to reach whoever is
alerting on broadcast failures. The retry is deliberately just one attempt,
not a backoff loop: this exercise is about the dispatch and aggregation
logic, and an unbounded retry loop here would turn a fast, bounded
broadcast into one that can hang on a truly dead replica.

Create `broadcaster.go`:

```go
package cachebroadcast

import (
	"errors"
	"fmt"
	"sync"
)

// ErrUnsupportedInvalidation is the sentinel for an invalidation kind the
// broadcaster does not recognize.
var ErrUnsupportedInvalidation = errors.New("unsupported invalidation kind")

// UpdateInvalidation tells replicas to set Key to Value.
type UpdateInvalidation struct{ Key, Value string }

// DeleteInvalidation tells replicas to remove Key.
type DeleteInvalidation struct{ Key string }

// PurgePrefixInvalidation tells replicas to drop every key under Prefix.
type PurgePrefixInvalidation struct{ Prefix string }

// Replica is one cache node reachable over the network. Each method can
// fail transiently, which is why Broadcast retries once before giving up on
// a given replica.
type Replica interface {
	Set(key, value string) error
	Delete(key string) error
	PurgePrefix(prefix string) error
}

// Broadcaster fans an invalidation out to a pool of replicas concurrently,
// deduplicating repeat deliveries of the same logical invalidation (a
// message bus may redeliver at-least-once) and tolerating one transient
// failure per replica via a single retry.
type Broadcaster struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// NewBroadcaster returns a Broadcaster with an empty dedup set.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{seen: make(map[string]struct{})}
}

// dedupKey derives the identity of an invalidation from its concrete type
// and fields. Two kinds can never collide because the key is prefixed by
// kind, so an UpdateInvalidation and a DeleteInvalidation for the same Key
// are treated as distinct events.
func dedupKey(inv any) (string, error) {
	switch i := inv.(type) {
	case UpdateInvalidation:
		return fmt.Sprintf("update:%s:%s", i.Key, i.Value), nil
	case DeleteInvalidation:
		return fmt.Sprintf("delete:%s", i.Key), nil
	case PurgePrefixInvalidation:
		return fmt.Sprintf("purge:%s", i.Prefix), nil
	default:
		return "", fmt.Errorf("%w: %T", ErrUnsupportedInvalidation, inv)
	}
}

// apply dispatches one invalidation to one replica's matching method.
func apply(inv any, r Replica) error {
	switch i := inv.(type) {
	case UpdateInvalidation:
		return r.Set(i.Key, i.Value)
	case DeleteInvalidation:
		return r.Delete(i.Key)
	case PurgePrefixInvalidation:
		return r.PurgePrefix(i.Prefix)
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedInvalidation, inv)
	}
}

// Broadcast applies inv to every replica concurrently. A repeat delivery of
// an invalidation already broadcast is a silent no-op, so an at-least-once
// message bus cannot double-apply it. Each replica gets one retry on
// failure; replicas that still fail after the retry are collected and
// returned together via errors.Join, so one flaky replica does not hide the
// failure of another.
func (b *Broadcaster) Broadcast(inv any, replicas []Replica) error {
	key, err := dedupKey(inv)
	if err != nil {
		return err
	}

	b.mu.Lock()
	if _, dup := b.seen[key]; dup {
		b.mu.Unlock()
		return nil
	}
	b.seen[key] = struct{}{}
	b.mu.Unlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(replicas))
	for _, r := range replicas {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := apply(inv, r); err != nil {
				if err := apply(inv, r); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
```

### The runnable demo

The demo's replicas write to a mutex-guarded `recorder` instead of printing
directly, and the demo sorts each broadcast's lines before printing them.
That is not incidental to the exercise: `Broadcast` delivers to replicas
concurrently by design, so two replicas' `fmt.Printf` calls racing straight
to stdout would produce a different interleaving on every run. Sorting each
batch is what makes the demo's output reproducible without changing
anything about `Broadcast` itself.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"

	"example.com/cache-invalidation-broadcaster"
)

// recorder collects one line per replica call, safely under the concurrent
// delivery Broadcast performs internally. The demo sorts each batch before
// printing so the output is deterministic regardless of goroutine
// scheduling order.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

func (r *recorder) since(start int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	batch := append([]string(nil), r.lines[start:]...)
	sort.Strings(batch)
	return batch
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

type namedReplica struct {
	name string
	rec  *recorder
}

func (r namedReplica) Set(key, value string) error {
	r.rec.add(fmt.Sprintf("%s: set %s=%s", r.name, key, value))
	return nil
}
func (r namedReplica) Delete(key string) error {
	r.rec.add(fmt.Sprintf("%s: delete %s", r.name, key))
	return nil
}
func (r namedReplica) PurgePrefix(prefix string) error {
	r.rec.add(fmt.Sprintf("%s: purge prefix %s", r.name, prefix))
	return nil
}

func main() {
	rec := &recorder{}
	b := cachebroadcast.NewBroadcaster()
	replicas := []cachebroadcast.Replica{
		namedReplica{name: "replica-a", rec: rec},
		namedReplica{name: "replica-b", rec: rec},
	}

	invalidations := []any{
		cachebroadcast.UpdateInvalidation{Key: "user:1", Value: "alice"},
		cachebroadcast.DeleteInvalidation{Key: "user:2"},
		cachebroadcast.PurgePrefixInvalidation{Prefix: "session:"},
	}
	for _, inv := range invalidations {
		start := rec.len()
		if err := b.Broadcast(inv, replicas); err != nil {
			fmt.Printf("broadcast failed: %v\n", err)
			continue
		}
		for _, line := range rec.since(start) {
			fmt.Println(line)
		}
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
replica-a: set user:1=alice
replica-b: set user:1=alice
replica-a: delete user:2
replica-b: delete user:2
replica-a: purge prefix session:
replica-b: purge prefix session:
```

### Tests

`fakeReplica` counts calls and fails its first N attempts per method,
guarded by its own mutex since `Broadcast` calls it from goroutines.

Create `broadcaster_test.go`:

```go
package cachebroadcast

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeReplica lets a test configure how many times each method should fail
// before it starts succeeding, and counts calls, safely under concurrent
// access since Broadcast delivers to replicas from goroutines.
type fakeReplica struct {
	mu                                sync.Mutex
	failSet, failDelete, failPurge    int
	setCalls, deleteCalls, purgeCalls int
}

func (f *fakeReplica) Set(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.failSet > 0 {
		f.failSet--
		return errors.New("transient set failure")
	}
	return nil
}

func (f *fakeReplica) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.failDelete > 0 {
		f.failDelete--
		return errors.New("transient delete failure")
	}
	return nil
}

func (f *fakeReplica) PurgePrefix(prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeCalls++
	if f.failPurge > 0 {
		f.failPurge--
		return errors.New("transient purge failure")
	}
	return nil
}

func (f *fakeReplica) counts() (set, del, purge int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCalls, f.deleteCalls, f.purgeCalls
}

func TestBroadcastDispatchesByKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		inv  any
		want func(*fakeReplica) bool
	}{
		{
			name: "update calls Set",
			inv:  UpdateInvalidation{Key: "user:1", Value: "alice"},
			want: func(r *fakeReplica) bool { s, d, p := r.counts(); return s == 1 && d == 0 && p == 0 },
		},
		{
			name: "delete calls Delete",
			inv:  DeleteInvalidation{Key: "user:1"},
			want: func(r *fakeReplica) bool { s, d, p := r.counts(); return s == 0 && d == 1 && p == 0 },
		},
		{
			name: "purge prefix calls PurgePrefix",
			inv:  PurgePrefixInvalidation{Prefix: "user:"},
			want: func(r *fakeReplica) bool { s, d, p := r.counts(); return s == 0 && d == 0 && p == 1 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := NewBroadcaster()
			r := &fakeReplica{}
			if err := b.Broadcast(tt.inv, []Replica{r}); err != nil {
				t.Fatalf("Broadcast: unexpected error %v", err)
			}
			if !tt.want(r) {
				s, d, p := r.counts()
				t.Fatalf("call counts set=%d delete=%d purge=%d did not match expectation", s, d, p)
			}
		})
	}
}

func TestBroadcastDeduplicatesRepeatDelivery(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster()
	r := &fakeReplica{}
	inv := UpdateInvalidation{Key: "user:1", Value: "alice"}

	if err := b.Broadcast(inv, []Replica{r}); err != nil {
		t.Fatalf("first broadcast: %v", err)
	}
	if err := b.Broadcast(inv, []Replica{r}); err != nil {
		t.Fatalf("second broadcast: %v", err)
	}
	if s, _, _ := r.counts(); s != 1 {
		t.Fatalf("Set called %d times, want exactly 1 (second delivery should be a no-op)", s)
	}
}

func TestBroadcastRetriesTransientFailureOnce(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster()
	r := &fakeReplica{failSet: 1}
	inv := UpdateInvalidation{Key: "user:1", Value: "alice"}

	if err := b.Broadcast(inv, []Replica{r}); err != nil {
		t.Fatalf("Broadcast: unexpected error %v (retry should have masked one failure)", err)
	}
	if s, _, _ := r.counts(); s != 2 {
		t.Fatalf("Set called %d times, want 2 (initial failure + retry)", s)
	}
}

func TestBroadcastReturnsJoinedErrorAfterExhaustingRetry(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster()
	healthy := &fakeReplica{}
	unhealthy := &fakeReplica{failSet: 10}
	inv := UpdateInvalidation{Key: "user:1", Value: "alice"}

	err := b.Broadcast(inv, []Replica{healthy, unhealthy})
	if err == nil {
		t.Fatal("expected an error from the permanently failing replica")
	}
	if hs, _, _ := healthy.counts(); hs != 1 {
		t.Fatalf("healthy replica Set called %d times, want 1", hs)
	}
	if us, _, _ := unhealthy.counts(); us != 2 {
		t.Fatalf("unhealthy replica Set called %d times, want 2 (initial + retry, both failing)", us)
	}
}

func TestBroadcastUnsupportedInvalidation(t *testing.T) {
	t.Parallel()
	b := NewBroadcaster()
	if err := b.Broadcast("not-an-invalidation", []Replica{&fakeReplica{}}); !errors.Is(err, ErrUnsupportedInvalidation) {
		t.Fatalf("Broadcast err = %v, want ErrUnsupportedInvalidation", err)
	}
}

// TestConcurrentBroadcastsDeduplicateAcrossGoroutines fires many concurrent
// Broadcast calls at the same Broadcaster for a mix of distinct and
// repeated invalidations, proving the dedup set is safe under real
// concurrent delivery and that each distinct key still reaches the replica
// exactly once.
func TestConcurrentBroadcastsDeduplicateAcrossGoroutines(t *testing.T) {
	b := NewBroadcaster()
	r := &fakeReplica{}
	const distinctKeys = 20
	const redeliveries = 5

	var wg sync.WaitGroup
	for k := 0; k < distinctKeys; k++ {
		inv := UpdateInvalidation{Key: fmt.Sprintf("user:%d", k), Value: "v"}
		for d := 0; d < redeliveries; d++ {
			wg.Add(1)
			go func(inv UpdateInvalidation) {
				defer wg.Done()
				if err := b.Broadcast(inv, []Replica{r}); err != nil {
					t.Errorf("Broadcast: unexpected error %v", err)
				}
			}(inv)
		}
	}
	wg.Wait()

	if s, _, _ := r.counts(); s != distinctKeys {
		t.Fatalf("Set called %d times, want exactly %d (one per distinct key, redeliveries deduped)", s, distinctKeys)
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Broadcast` is correct because the dedup check happens under the lock and
*before* any goroutines are spawned, so two concurrent redeliveries of the
same invalidation cannot both pass the check and both fan out — one of them
observes `dup == true` and returns immediately. The retry-then-join
sequence is the other property worth protecting: a version that returned on
the *first* failure instead of retrying once would treat routine transient
blips as broadcast failures, and a version that swallowed a permanently
failing replica's error instead of joining it would leave that replica
silently stale forever. The concurrency test is what actually exercises the
dedup set's thread safety — a single-goroutine test calling `Broadcast`
twice in sequence would pass even with an unguarded `map[string]struct{}`,
since Go's map corruption under concurrent access only surfaces under real
concurrent access, which is exactly what `-race` is verifying here.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector)
- [AWS ElastiCache: cache invalidation strategies](https://aws.amazon.com/caching/invalidation/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-distributed-consensus-handler.md](22-distributed-consensus-handler.md) | Next: [24-transaction-log-recovery.md](24-transaction-log-recovery.md)
