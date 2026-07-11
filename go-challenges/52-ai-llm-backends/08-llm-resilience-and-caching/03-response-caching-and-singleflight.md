# Exercise 3: Response Cache with Request Coalescing and TTL/LRU Eviction

The final layer is the one that saves the most money: a cache that serves identical
prompts from memory instead of paying the provider again, backed by request
coalescing so a burst of identical concurrent prompts collapses to a single upstream
call. This exercise builds that caching decorator over the `Completer` seam, with a
stable content-hash key, bounded memory and staleness, and short-lived negative
caching.

This module is fully self-contained: its own `go mod init`, the `Completer` seam,
and fakes for every path. Nothing here imports another exercise. It depends on two
external modules — `golang.org/x/sync/singleflight` and
`github.com/hashicorp/golang-lru/v2/expirable` — so the offline gate fails at the
build step until those are fetched; the logic is what matters and is proven by the
tests once they are.

## What you'll build

```text
llmcache/                    independent module: example.com/llmcache
  go.mod                     requires golang.org/x/sync + hashicorp/golang-lru/v2
  caching.go                 CachingCompleter, cacheKey, Stats; TTL/LRU + singleflight
  cmd/
    demo/
      main.go                runnable demo: hit/miss counters over a scripted sequence
  caching_test.go            key stability, cache hit, coalescing, eviction, negative cache
```

- Files: `caching.go`, `cmd/demo/main.go`, `caching_test.go`.
- Implement: a `CachingCompleter` that keys on a `sha256` hash of a canonical `json.Marshal` of the output-affecting fields, serves hits from an `expirable.LRU` (bounded size + TTL), routes misses through `singleflight.Group.Do`, negatively caches terminal client errors with a short TTL, and exposes hit/miss/coalesced counters.
- Test: two identical requests share a key and the second is a cache hit (one upstream call); a temperature change yields a different key; K concurrent goroutines on one key collapse to a single upstream call and all get the same value; size and TTL bound the cache; a terminal error is negatively cached.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/llmcache/cmd/demo
cd ~/go-exercises/llmcache
go mod init example.com/llmcache
go mod edit -go=1.26
go get golang.org/x/sync/singleflight
go get github.com/hashicorp/golang-lru/v2/expirable
```

### The cache key must be canonical

Two requests are the same only if every field that affects the model's output is the
same: model, system prompt, messages, temperature, and max tokens. The key is a
`sha256` over a `json.Marshal` of exactly those fields, hex-encoded. Two subtleties
decide whether the cache ever hits. First, serialize a *struct*, not a *map*: Go
randomizes map iteration order, so a map-based key would differ run to run and never
hit. A struct marshals its fields in declaration order, deterministically. Second,
include *only* output-affecting fields: fold in a request ID or a timestamp and the
key changes every call so you never hit; drop temperature and you would serve a
temperature-0 answer to a temperature-0.9 request. `keyFields` names the exact set.

### The cache handles repeats; singleflight handles simultaneity

These are two different problems. An `expirable.LRU` absorbs *repeats over time*:
the same prompt asked again a minute later is served from memory. But on a cold key,
a burst of concurrent requests all miss the cache at once and would all call
upstream — a stampede. `singleflight.Group.Do` absorbs that *simultaneity*: it runs
the miss function once per key and hands the single result to every concurrent
caller, reporting `shared == true` to every caller in a coalesced group (leader
included). The miss path is
therefore wrapped in `Do` keyed by the same cache key, so 500 identical concurrent
prompts become one upstream call and 499 shared results.

### Bounded memory, bounded staleness, and negative caching

`expirable.NewLRU(size, onEvict, ttl)` bounds both axes in one thread-safe
structure: the size cap evicts the least-recently-used entry so memory cannot grow
without bound, and the TTL expires entries so a stale completion is never served
past its window. Negative caching stores *failures* briefly: a request that fails
with a terminal client error (400, 401, 403, 404, 422) will fail identically on
every retry, so caching that failure for a few seconds stops a broken prompt from
hammering upstream. Its TTL is deliberately much shorter than the success TTL,
because the underlying cause (a fixed prompt, a rotated key) may resolve at any
moment. Transient errors and context cancellations are never negatively cached — a
503 or a cancelled request says nothing durable about the input.

### Counters via atomics

Hit, miss, and coalesced counters are incremented from many goroutines, so they are
`atomic.Int64`. They make the cache observable: in production you would export them
as metrics to see the hit rate and the coalescing rate, which together tell you how
much money the cache is saving.

Create `caching.go`:

```go
package llmcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// Message is one turn in a conversation.
type Message struct {
	Role    string
	Content string
}

// Request is a provider-agnostic completion request.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Temperature float64
	MaxTokens   int
}

// Response is a provider-agnostic completion response.
type Response struct {
	Text  string
	Model string
}

// Completer is the seam the cache decorates.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// APIError is a provider-agnostic transport error carrying the HTTP status.
type APIError struct {
	StatusCode int
	Err        error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d (%s)", e.StatusCode, http.StatusText(e.StatusCode))
}

func (e *APIError) Unwrap() error { return e.Err }

// keyFields is the canonical, output-affecting subset of a Request. Only these
// fields participate in the cache key; anything else (a request ID, a timestamp)
// would make the key volatile and defeat the cache.
type keyFields struct {
	Model       string
	System      string
	Messages    []Message
	Temperature float64
	MaxTokens   int
}

// cacheKey is a stable hash over a canonical serialization of the request. It
// marshals a struct (deterministic field order), not a map (randomized).
func cacheKey(req Request) string {
	b, _ := json.Marshal(keyFields{
		Model:       req.Model,
		System:      req.System,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// terminalClientError reports whether err is a client error that will fail
// identically forever and is therefore worth negative-caching.
func terminalClientError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadRequest, // 400
			http.StatusUnauthorized,        // 401
			http.StatusForbidden,           // 403
			http.StatusNotFound,            // 404
			http.StatusUnprocessableEntity: // 422
			return true
		}
	}
	return false
}

// CachingCompleter is a caching decorator over a Completer.
type CachingCompleter struct {
	next     Completer
	cache    *expirable.LRU[string, Response]
	negCache *expirable.LRU[string, error]
	group    singleflight.Group

	hits      atomic.Int64
	misses    atomic.Int64
	coalesced atomic.Int64
}

var _ Completer = (*CachingCompleter)(nil)

// NewCachingCompleter builds a cache bounded to size entries, serving success
// entries for ttl and negatively-cached failures for negTTL.
func NewCachingCompleter(next Completer, size int, ttl, negTTL time.Duration) *CachingCompleter {
	return &CachingCompleter{
		next:     next,
		cache:    expirable.NewLRU[string, Response](size, nil, ttl),
		negCache: expirable.NewLRU[string, error](size, nil, negTTL),
	}
}

// Complete serves a cache hit when possible, else collapses concurrent misses for
// the same key into a single upstream call via singleflight.
func (c *CachingCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	key := cacheKey(req)

	if resp, ok := c.cache.Get(key); ok {
		c.hits.Add(1)
		return resp, nil
	}
	if cerr, ok := c.negCache.Get(key); ok {
		c.hits.Add(1)
		return Response{}, cerr
	}
	c.misses.Add(1)

	v, err, shared := c.group.Do(key, func() (any, error) {
		resp, err := c.next.Complete(ctx, req)
		if err != nil {
			if terminalClientError(err) {
				c.negCache.Add(key, err)
			}
			return Response{}, err
		}
		c.cache.Add(key, resp)
		return resp, nil
	})
	if shared {
		c.coalesced.Add(1)
	}
	if err != nil {
		return Response{}, err
	}
	return v.(Response), nil
}

// Stats reports the running hit, miss, and coalesced counts.
func (c *CachingCompleter) Stats() (hits, misses, coalesced int64) {
	return c.hits.Load(), c.misses.Load(), c.coalesced.Load()
}

// Len reports the number of live success entries.
func (c *CachingCompleter) Len() int { return c.cache.Len() }
```

### The runnable demo

The demo runs a scripted, sequential sequence so the counters are deterministic:
the same request twice (a miss then a hit), then the same messages with a different
temperature (a different key, so a miss).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/llmcache"
)

// provider is a fake upstream that counts calls.
type provider struct {
	calls int
}

func (p *provider) Complete(_ context.Context, _ llmcache.Request) (llmcache.Response, error) {
	p.calls++
	return llmcache.Response{Text: "the answer is 42", Model: "demo-model"}, nil
}

func main() {
	p := &provider{}
	c := llmcache.NewCachingCompleter(p, 256, time.Minute, 5*time.Second)
	ctx := context.Background()

	req := llmcache.Request{
		Model:    "demo-model",
		Messages: []llmcache.Message{{Role: "user", Content: "what is 6 times 7?"}},
	}
	r1, _ := c.Complete(ctx, req)
	r2, _ := c.Complete(ctx, req) // identical: cache hit

	hot := req
	hot.Temperature = 0.9
	r3, _ := c.Complete(ctx, hot) // different key: miss

	h, m, co := c.Stats()
	fmt.Printf("r1=%q r2=%q r3=%q\n", r1.Text, r2.Text, r3.Text)
	fmt.Printf("hits=%d misses=%d coalesced=%d upstream=%d\n", h, m, co, p.calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
r1="the answer is 42" r2="the answer is 42" r3="the answer is 42"
hits=1 misses=2 coalesced=0 upstream=2
```

### Tests

`TestKeyStability` proves identical requests share a key and a temperature change
splits it. `TestCacheHit` proves the second identical call is served from cache with
one upstream call. `TestCoalescing` fires K goroutines at one blocked key and proves
a single upstream call and a shared result. `TestEvictionSize` proves the size cap
bounds `Len`. `TestTTLExpiry` proves an entry expires. `TestNegativeCache` proves a
terminal error is cached briefly.

Create `caching_test.go`:

```go
package llmcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingCompleter records calls and can block to force overlap.
type countingCompleter struct {
	mu      sync.Mutex
	calls   int
	resp    Response
	err     error
	block   chan struct{}
	entered chan struct{}
}

func (f *countingCompleter) Complete(_ context.Context, _ Request) (Response, error) {
	f.mu.Lock()
	f.calls++
	resp, err, block, entered := f.resp, f.err, f.block, f.entered
	f.mu.Unlock()

	if block != nil {
		if entered != nil {
			entered <- struct{}{}
		}
		<-block
	}
	if err != nil {
		return Response{}, err
	}
	return resp, nil
}

func (f *countingCompleter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func userReq(content string) Request {
	return Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: content}},
	}
}

func TestKeyStability(t *testing.T) {
	t.Parallel()
	a := userReq("hello")
	b := userReq("hello")
	if cacheKey(a) != cacheKey(b) {
		t.Fatal("identical requests produced different keys")
	}
	hot := a
	hot.Temperature = 0.9
	if cacheKey(a) == cacheKey(hot) {
		t.Fatal("temperature change did not change the key")
	}
}

func TestCacheHit(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{resp: Response{Text: "cached"}}
	c := NewCachingCompleter(fake, 128, time.Minute, time.Second)
	ctx := context.Background()
	req := userReq("q")

	if _, err := c.Complete(ctx, req); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.Complete(ctx, req); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := fake.Calls(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second is a cache hit)", got)
	}
	if h, m, _ := c.Stats(); h != 1 || m != 1 {
		t.Fatalf("hits,misses = %d,%d; want 1,1", h, m)
	}
}

func TestDifferentKeysMiss(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{resp: Response{Text: "x"}}
	c := NewCachingCompleter(fake, 128, time.Minute, time.Second)
	ctx := context.Background()

	a := userReq("q")
	b := a
	b.Temperature = 0.9
	_, _ = c.Complete(ctx, a)
	_, _ = c.Complete(ctx, b)

	if got := fake.Calls(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (different keys)", got)
	}
}

func TestCoalescing(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{})
	fake := &countingCompleter{resp: Response{Text: "one"}, block: release, entered: entered}
	c := NewCachingCompleter(fake, 128, time.Minute, time.Second)
	req := userReq("q")

	const K = 50
	results := make([]Response, K)
	var wg sync.WaitGroup
	for i := range K {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := c.Complete(context.Background(), req)
			results[i] = r
		}()
	}

	<-entered                         // the single leader is inside upstream
	time.Sleep(20 * time.Millisecond) // let the rest join the in-flight call
	close(release)
	wg.Wait()

	if got := fake.Calls(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (coalesced)", got)
	}
	for i := range K {
		if results[i].Text != "one" {
			t.Fatalf("result %d = %q, want one", i, results[i].Text)
		}
	}
	if _, _, co := c.Stats(); co == 0 {
		t.Fatal("coalesced count = 0, want > 0")
	}
}

func TestEvictionSize(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{resp: Response{Text: "x"}}
	c := NewCachingCompleter(fake, 3, time.Minute, time.Second)
	ctx := context.Background()

	for i := range 6 {
		_, _ = c.Complete(ctx, userReq(fmt.Sprintf("q%d", i)))
	}
	if got := c.Len(); got > 3 {
		t.Fatalf("Len = %d, want <= 3 (size cap)", got)
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{resp: Response{Text: "x"}}
	c := NewCachingCompleter(fake, 128, 20*time.Millisecond, time.Second)
	ctx := context.Background()
	req := userReq("q")

	_, _ = c.Complete(ctx, req)
	time.Sleep(40 * time.Millisecond) // past the TTL
	_, _ = c.Complete(ctx, req)

	if got := fake.Calls(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (entry expired)", got)
	}
}

func TestNegativeCache(t *testing.T) {
	t.Parallel()
	fake := &countingCompleter{err: &APIError{StatusCode: 400}}
	c := NewCachingCompleter(fake, 128, time.Minute, time.Second)
	ctx := context.Background()
	req := userReq("bad")

	_, err1 := c.Complete(ctx, req)
	_, err2 := c.Complete(ctx, req)

	var apiErr *APIError
	if !errors.As(err1, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("err1 = %v, want *APIError 400", err1)
	}
	if !errors.As(err2, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("err2 = %v, want negatively-cached *APIError 400", err2)
	}
	if got := fake.Calls(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (second served from negative cache)", got)
	}
}

func Example() {
	fake := &countingCompleter{resp: Response{Text: "answer"}}
	c := NewCachingCompleter(fake, 128, time.Minute, time.Second)
	ctx := context.Background()
	req := userReq("hi")

	_, _ = c.Complete(ctx, req) // miss
	_, _ = c.Complete(ctx, req) // hit
	hot := req
	hot.Temperature = 0.9
	_, _ = c.Complete(ctx, hot) // different key: miss

	h, m, _ := c.Stats()
	fmt.Printf("hits=%d misses=%d upstream=%d\n", h, m, fake.Calls())
	// Output: hits=1 misses=2 upstream=2
}
```

## Review

The cache is correct when the key is a pure function of the output-affecting fields
and the layers compose in the right order. `TestKeyStability` pins the key: identical
requests hash equal, a temperature change hashes differently — because the key is a
`sha256` over a struct's canonical JSON, not a map. `TestCacheHit` and
`TestDifferentKeysMiss` prove the cache serves repeats and separates distinct
inputs. `TestCoalescing`, run under `-race`, proves the miss path collapses a
concurrent burst to a single upstream call and shares the result. `TestEvictionSize`
and `TestTTLExpiry` prove memory and staleness are both bounded, and
`TestNegativeCache` proves a terminal error is cached briefly so a broken prompt does
not hammer upstream.

The mistakes to avoid: building the key from a `map` or `fmt.Sprintf("%v")` so it is
unstable and never hits; including a volatile field so it also never hits; caching
without singleflight so a burst still stampedes; using an unbounded `map` (a memory
leak) or an LRU with no TTL (hours-stale answers); and negatively caching transient
errors or cancellations, which say nothing durable about the input. Because this
module depends on `golang.org/x/sync` and `github.com/hashicorp/golang-lru/v2`, the
offline gate fails at build until they are in the module cache; once fetched, the
fakes make every behavior deterministic and `go test -race` proves the concurrent
paths.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — `Group.Do` and its `shared` return for collapsing concurrent identical calls.
- [`hashicorp/golang-lru/v2/expirable`](https://pkg.go.dev/github.com/hashicorp/golang-lru/v2/expirable) — `NewLRU` with a size cap, TTL, and evict callback in one thread-safe structure.
- [`crypto/sha256`](https://pkg.go.dev/crypto/sha256) — `Sum256` for a stable content hash.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — struct marshaling for a canonical, deterministic key serialization.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-timeout-budgets-and-circuit-breaker.md](02-timeout-budgets-and-circuit-breaker.md) | Next: [../../53-wasm-and-extensibility/01-wazero-host-runtime/00-concepts.md](../../53-wasm-and-extensibility/01-wazero-host-runtime/00-concepts.md)
