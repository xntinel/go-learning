# Exercise 32: Coalesce Identical In-Flight Requests via Goroutine Fanout

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A cache entry expiring under heavy read traffic produces the classic
thundering herd: dozens or hundreds of concurrent requests all miss the
cache at once and all turn around and hit the same expensive backend call
— the same database query, the same upstream API — for the exact same
key, at the exact same moment. The backend sees a traffic spike shaped
exactly like the cache's blind spot, not like real demand. The fix, known
as request coalescing or "singleflight," is to let only the first caller
for a key actually perform the expensive work while every other caller
that arrives before it finishes waits on that same in-flight call and
reuses its result. Getting this right under real concurrency, with a
timeout attached to each individual caller, is subtler than it looks: a
caller's own timeout must never cancel the work another caller is still
legitimately waiting on. This module is fully self-contained: its own `go
mod init`, all code inline, its own demo and tests.

## What you'll build

```text
request-coalescing-singleflight/   independent module: example.com/request-coalescing-singleflight
  go.mod                           go 1.24
  coalesce.go                      (*Group).Do; Fetch(ctx, g, key, fn) any
  cmd/
    demo/
      main.go                      five concurrent callers coalesce onto one in-flight call
  coalesce_test.go                   concurrent claims, error propagation, timeout fairness
```

- Files: `coalesce.go`, `cmd/demo/main.go`, `coalesce_test.go`.
- Implement: `(*Group).Do(key string, fn func() (any, error)) (val any, err
  error, shared bool)`; `Fetch(ctx context.Context, g *Group, key string, fn
  func() (any, error)) any` type-switching the caller's outcome into
  `Success`, `Failure`, or `TimedOut`.
- Test: many concurrent callers for one key coalesce onto exactly one `fn`
  invocation with all but one reporting `shared`, every coalesced caller
  receives the same propagated error, and a caller with a short timeout
  reports `TimedOut` while a caller coalesced onto the same call with a
  longer timeout still reports `Success`.

Set up the module:

```bash
go mod edit -go=1.24
```

`Group.Do` registers a `call` into `g.calls[key]` and checks whether one is
already there, both under the same lock acquisition — that is the entire
correctness argument for coalescing. If checking "does a call for this key
exist" and creating "there is now a call for this key" were two separate
critical sections, two callers arriving at nearly the same instant could
both see no existing call and both proceed to invoke `fn`, defeating the
whole point. Once a `call` is registered, every other caller for that key
simply blocks on its `done` channel, which is closed exactly once, after
`fn` returns — so waking up from `<-c.done` and reading `c.val`/`c.err`
afterward is safe by the same happens-before guarantee any channel close
provides, without needing a second lock to protect those two fields.
`Fetch` layers a per-caller timeout on top of `Do` by running it in its own
goroutine and racing its completion against `ctx.Done()`: this is
deliberate, because it means a caller's own context expiring does not, and
must not, cancel the `fn` invocation another caller is still legitimately
waiting on — it only changes what *this* caller reports back. That
asymmetry is what "timeout fairness" means here: one slow caller giving up
early can never take down the result for everyone else still waiting.

Create `coalesce.go`:

```go
package coalesce

import (
	"context"
	"sync"
)

// call is the in-flight (or just-completed) state shared by every caller
// that asked for the same key while fn was running.
type call struct {
	done chan struct{}
	val  any
	err  error
}

// Group deduplicates concurrent calls that share a key: the first caller
// for a key actually runs fn; every other caller that arrives before it
// finishes waits on the same call and reuses its result instead of running
// fn again. This is the classic "singleflight" pattern, used to protect a
// backing store from a thundering herd of identical concurrent requests —
// a cache stampede on a just-expired key, for instance.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// NewGroup returns a Group ready to use.
func NewGroup() *Group {
	return &Group{calls: make(map[string]*call)}
}

// Do executes fn for key, or waits for and reuses the result of an
// identical call already in flight. shared reports whether the caller
// received a result computed for someone else's call rather than its own.
// Registering the call and checking for an existing one happen under the
// same lock, so two callers racing to be "first" for a key can never both
// believe they are first and both invoke fn.
func (g *Group) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &call{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.val, c.err, false
}

// Success wraps a value fn produced.
type Success struct{ Value any }

// Failure wraps an error fn returned.
type Failure struct{ Err error }

// TimedOut means ctx's deadline elapsed before this caller's coalesced call
// finished.
type TimedOut struct{}

// Fetch runs fn — deduplicated through g by key — and classifies the
// outcome as Success, Failure, or TimedOut against ctx's deadline. Because
// coalesced callers share one underlying call, a slow caller's context
// timing out does not cancel the in-flight fn for whichever caller is
// actually waiting on it to complete normally: it only changes what this
// particular caller reports back, which is what makes timeout fairness
// possible — one caller can give up early while the call it was
// piggybacking on keeps running for everyone still waiting.
func Fetch(ctx context.Context, g *Group, key string, fn func() (any, error)) any {
	result := make(chan struct{})
	var val any
	var err error
	go func() {
		val, err, _ = g.Do(key, fn)
		close(result)
	}()

	select {
	case <-result:
		if err != nil {
			return Failure{Err: err}
		}
		return Success{Value: val}
	case <-ctx.Done():
		return TimedOut{}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/request-coalescing-singleflight"
)

func main() {
	g := coalesce.NewGroup()
	var calls int
	var mu sync.Mutex
	release := make(chan struct{})

	fn := func() (any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release // stay in flight until the demo releases every caller at once
		return "expensive-result", nil
	}

	const callers = 5
	results := make([]string, callers)
	shared := make([]bool, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val, _, isShared := g.Do("expensive-key", fn)
			results[i] = val.(string)
			shared[i] = isShared
		}(i)
	}

	// Give every caller time to reach g.Do and either start fn or join the
	// call already in flight, before fn is allowed to return. There is no
	// synchronization primitive that reports "a goroutine is now blocked on
	// a channel receive," so a short, generous sleep stands in for one —
	// the same rendezvous idiom singleflight implementations use in their
	// own tests.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	fmt.Printf("fn invoked %d time(s) for %d concurrent callers\n", calls, callers)
	sharedCount := 0
	for _, s := range shared {
		if s {
			sharedCount++
		}
	}
	fmt.Printf("%d of %d callers received a shared (deduplicated) result\n", sharedCount, callers)
	fmt.Println("value:", results[0])
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
fn invoked 1 time(s) for 5 concurrent callers
4 of 5 callers received a shared (deduplicated) result
value: expensive-result
```

All five goroutines call `g.Do` while `fn` is still blocked on `release`,
so exactly one of them registers the call and runs `fn`, and the other
four coalesce onto it — reporting `shared=true` and receiving the same
`"expensive-result"` value without the backend ever seeing a second call.

### Tests

Every timing-sensitive test here uses the same rendezvous idiom as the
demo: a channel-gated `fn` that stays in flight until the test releases
it, combined with a short, generous sleep to let concurrent callers reach
their blocking point before release. This is the same pattern
`golang.org/x/sync/singleflight`'s own test suite uses, because there is no
synchronization primitive that reports "a goroutine is now blocked on a
channel receive" — only a bounded wait can stand in for one.

Create `coalesce_test.go`:

```go
package coalesce

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestDoCoalescesConcurrentCallers proves that many concurrent callers for
// the same key, all arriving while fn is still in flight, cause fn to run
// exactly once and every caller but one to receive a shared result.
func TestDoCoalescesConcurrentCallers(t *testing.T) {
	g := NewGroup()
	release := make(chan struct{})
	fn := func() (any, error) {
		return waitAndReturn(release, "result")
	}

	const callers = 10
	results := make([]any, callers)
	shared := make([]bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val, _, isShared := g.Do("key", fn)
			results[i] = val
			shared[i] = isShared
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	sharedCount := 0
	for i, s := range shared {
		if results[i] != "result" {
			t.Fatalf("caller %d got %v, want %q", i, results[i], "result")
		}
		if s {
			sharedCount++
		}
	}
	if sharedCount != callers-1 {
		t.Fatalf("shared count = %d, want %d (all but the one that actually ran fn)", sharedCount, callers-1)
	}
}

// TestDoPropagatesError proves every coalesced caller receives the same
// error fn returned, not just the caller that happened to run it.
func TestDoPropagatesError(t *testing.T) {
	g := NewGroup()
	wantErr := errors.New("upstream unavailable")
	release := make(chan struct{})
	fn := func() (any, error) {
		<-release
		return nil, wantErr
	}

	const callers = 5
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err, _ := g.Do("key", fn)
			errs[i] = err
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Fatalf("caller %d err = %v, want %v", i, err, wantErr)
		}
	}
}

// TestFetchTimeoutFairness proves that one caller's short timeout does not
// affect a different caller coalesced onto the same in-flight call: the
// short-timeout caller reports TimedOut while the long-timeout caller still
// reports Success once the call finishes.
func TestFetchTimeoutFairness(t *testing.T) {
	g := NewGroup()
	release := make(chan struct{})
	fn := func() (any, error) {
		<-release
		return "value", nil
	}

	var shortResult, longResult any
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		shortResult = Fetch(ctx, g, "key", fn)
	}()

	time.Sleep(5 * time.Millisecond) // let the short caller start fn first
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		longResult = Fetch(ctx, g, "key", fn)
	}()

	time.Sleep(100 * time.Millisecond) // well past the short timeout, well under the long one
	close(release)
	wg.Wait()

	if _, ok := shortResult.(TimedOut); !ok {
		t.Fatalf("shortResult = %#v, want TimedOut", shortResult)
	}
	success, ok := longResult.(Success)
	if !ok {
		t.Fatalf("longResult = %#v, want Success", longResult)
	}
	if success.Value != "value" {
		t.Fatalf("longResult.Value = %v, want %q", success.Value, "value")
	}
}

func waitAndReturn(release chan struct{}, val any) (any, error) {
	<-release
	return val, nil
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Do` is correct because the map lookup and the map insert both happen while
holding `g.mu`, with `fn` invoked only after that lock is released — running
`fn` while still holding the lock would serialize every key's expensive
work behind a single mutex instead of only serializing callers that share a
key. `Fetch` is correct because it never touches the underlying call's
lifecycle from the timeout path: `ctx.Done()` firing changes only what the
current goroutine returns, never anything inside `Do` itself, which is
exactly what the timeout-fairness test verifies by driving two callers with
deliberately different deadlines through the same in-flight call and
checking that they diverge in their own outcome without affecting each
other's. The trap this design avoids is coupling cancellation to the
shared call: an implementation that plumbed `ctx` into `fn` itself and
canceled it on the first caller's timeout would silently fail every other
coalesced caller too, defeating the entire reason to coalesce in the first
place.

## Resources

- [golang.org/x/sync/singleflight](https://pkg.go.dev/golang.org/x/sync/singleflight)
- [context package](https://pkg.go.dev/context)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-vector-clock-causal-ordering.md](31-vector-clock-causal-ordering.md) | Next: [33-watermark-stream-processor.md](33-watermark-stream-processor.md)
