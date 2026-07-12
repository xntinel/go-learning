# Exercise 1: Sliding-Window Limiter with an Atomic Lua Script

A distributed rate limiter lives or dies on one property: the read-decide-write
must be indivisible on the server. This exercise builds a Redis-backed `Limiter`
that offers both a fixed-window counter and a sliding-window log, each implemented
as a single atomic Lua script, so two concurrent callers can never both pass a
stale check.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. The tests run against an in-process `miniredis` server (which speaks
`EVAL`, sorted sets, and a controllable clock), so no real Redis is needed for the
default test path; an integration test behind a build tag targets a real Redis.

## What you'll build

```text
rlwindow/                        independent module: example.com/rlwindow
  go.mod                         go 1.26; requires go-redis/v9, miniredis/v2
  limiter.go                     Decision, Limiter, NewFixedWindow, NewSlidingWindow, Allow (two Lua scripts)
  cmd/
    demo/
      main.go                    starts miniredis, drives 5 requests through a limit of 3
  limiter_test.go                miniredis tests: limit, reset, boundary burst, concurrency, Example
  limiter_integration_test.go    //go:build integration — same assertions against REDIS_ADDR
```

Files: `limiter.go`, `cmd/demo/main.go`, `limiter_test.go`, `limiter_integration_test.go`.
Implement: a `Limiter` with `NewFixedWindow` and `NewSlidingWindow` constructors and one `Allow(ctx, key) (Decision, error)` method that runs a single Lua `EVAL` per call.
Test: N requests inside a window flip `Allowed` to false at the limit; advancing the clock resets the quota; the fixed window admits a boundary burst the sliding window rejects; concurrent goroutines never exceed the limit.
Verify: `go test -race ./...` offline against miniredis; `go test -tags integration -race ./...` against a real Redis via `REDIS_ADDR`.

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/10-redis-rate-limiting/01-sliding-window-atomic-lua/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/10-redis-rate-limiting/01-sliding-window-atomic-lua
go mod edit -go=1.26
go get github.com/redis/go-redis/v9
go get github.com/alicebob/miniredis/v2
```

### Why the whole decision is one script

The limiter answers "does this request fit in the current window?" by counting
what has already happened, deciding, and recording this request. If those steps
are separate round trips, two requests racing through the same key can both read
the same count, both decide they fit, and both record — and the limit is breached.
The fix is to send the count-decide-record as one Lua script: Redis runs it
atomically, so no other command interleaves and the branch on the count is taken
against a value nothing else can change mid-flight. `redis.NewScript` wraps the
source; `(*Script).Run` issues `EVALSHA` with the cached hash and falls back to
`EVAL` with the full body the first time, so the script body crosses the wire
once, not per request.

Two scripts, two algorithms. The **fixed-window** script is one `INCR` plus a
`PEXPIRE` on the first hit of a window; it is O(1) in memory and time but permits
a burst of up to 2x the limit across the window boundary. The **sliding-window
log** script keeps a sorted set of request timestamps: it prunes entries older
than the window, counts the rest, and appends the new one only if there is room —
exact, at the cost of O(requests) memory per key.

### The server clock and unique members

Both scripts read the clock from Redis itself with `redis.call('TIME')` rather
than trusting the caller's wall clock, so a client whose clock drifts cannot skew
its own window — every replica scores entries against one authority. `TIME`
returns `{seconds, microseconds}`; the script folds that into milliseconds to
match `PEXPIRE` and the ZSET score resolution.

The sorted-set member must be unique per request, or two requests landing in the
same millisecond would `ZADD` the same member and the second would overwrite the
first, undercounting. The clock is the server's job (the *score*), but uniqueness
is the caller's job (the *member*): the limiter generates a unique member token
from a process-local atomic counter plus a random suffix and passes it as an
argument. This cleanly separates the skew-proof clock from member uniqueness.

### The Decision type and Allow

`Allow` returns a `Decision{Allowed, Remaining, RetryAfter}`. The script returns a
three-element array — `{allowed, remaining, retryAfterMs}` — which go-redis hands
back as an `[]interface{}` of `int64`s via `(*Cmd).Slice()`. `RetryAfter` is zero
on an allowed request and, on a denied one, the time until the oldest in-window
entry ages out (sliding window) or the key's remaining TTL (fixed window).

Create `limiter.go`:

```go
package ratelimit

import (
	"context"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Decision is the outcome of a single Allow call.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter enforces a request quota for a key against Redis. The count-decide-record
// sequence runs as one atomic Lua script, so concurrent callers cannot both pass a
// stale check.
type Limiter struct {
	rdb       redis.Scripter
	limit     int
	window    time.Duration
	script    *redis.Script
	useMember bool
	seq       uint64
}

// NewFixedWindow limits to limit requests per fixed window. Cheap (one INCR plus a
// PEXPIRE) but permits up to 2x the limit across the window boundary.
func NewFixedWindow(rdb redis.Scripter, limit int, window time.Duration) *Limiter {
	return &Limiter{
		rdb:    rdb,
		limit:  limit,
		window: window,
		script: redis.NewScript(fixedWindowSrc),
	}
}

// NewSlidingWindow limits to limit requests over a rolling window using a sorted
// set of timestamps. Exact (no boundary burst) at O(requests) memory per key.
func NewSlidingWindow(rdb redis.Scripter, limit int, window time.Duration) *Limiter {
	return &Limiter{
		rdb:       rdb,
		limit:     limit,
		window:    window,
		script:    redis.NewScript(slidingWindowSrc),
		useMember: true,
	}
}

// member returns a unique sorted-set member for one request: a random base-36
// value plus a process-local counter, so two requests in the same millisecond do
// not collide.
func (l *Limiter) member() string {
	n := atomic.AddUint64(&l.seq, 1)
	return strconv.FormatUint(rand.Uint64(), 36) + "-" + strconv.FormatUint(n, 36)
}

// Allow runs the limiter's script once for key and reports the decision.
func (l *Limiter) Allow(ctx context.Context, key string) (Decision, error) {
	args := []any{l.limit, l.window.Milliseconds()}
	if l.useMember {
		args = append(args, l.member())
	}
	res, err := l.script.Run(ctx, l.rdb, []string{key}, args...).Slice()
	if err != nil {
		return Decision{}, err
	}
	allowed, _ := res[0].(int64)
	remaining, _ := res[1].(int64)
	retryMs, _ := res[2].(int64)
	return Decision{
		Allowed:    allowed == 1,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(retryMs) * time.Millisecond,
	}, nil
}

// fixedWindowSrc counts a fixed window with INCR and expires the key after one
// window. KEYS[1]=key, ARGV[1]=limit, ARGV[2]=windowMillis. Returns
// {allowed, remaining, retryAfterMillis}.
const fixedWindowSrc = `
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local count = redis.call('INCR', KEYS[1])
if count == 1 then
	redis.call('PEXPIRE', KEYS[1], window)
end
if count > limit then
	local ttl = redis.call('PTTL', KEYS[1])
	if ttl < 0 then ttl = 0 end
	return {0, 0, ttl}
end
return {1, limit - count, 0}
`

// slidingWindowSrc keeps a sorted set of request timestamps scored by the Redis
// server clock. KEYS[1]=key, ARGV[1]=limit, ARGV[2]=windowMillis, ARGV[3]=member.
// Returns {allowed, remaining, retryAfterMillis}.
const slidingWindowSrc = `
local limit  = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local member = ARGV[3]
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now - window)
local count = redis.call('ZCARD', KEYS[1])
if count < limit then
	redis.call('ZADD', KEYS[1], now, member)
	redis.call('PEXPIRE', KEYS[1], window)
	return {1, limit - count - 1, 0}
end
local oldest = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')
local retry = 0
if oldest[2] ~= nil then
	retry = math.ceil((tonumber(oldest[2]) + window) - now)
	if retry < 0 then retry = 0 end
end
return {0, 0, retry}
`
```

### The runnable demo

The demo starts an in-process `miniredis` so it runs with no external Redis, then
drives five requests through a fixed window of three and prints each decision. The
first three are admitted with a decreasing remaining count; the last two are
rejected.

Note the import path and package name differ on purpose: the module is
`example.com/rlwindow` (matching the directory), but the package declared in
`limiter.go` is `ratelimit`. Go binds an import to the package *name* declared in
its files, not to the last path segment, so `import "example.com/rlwindow"` makes
the identifiers available as `ratelimit.NewFixedWindow` and `ratelimit.Decision`.
That is why the demo imports the `rlwindow` path yet calls `ratelimit.*`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"example.com/rlwindow"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	lim := ratelimit.NewFixedWindow(rdb, 3, time.Minute)
	ctx := context.Background()

	fmt.Println("fixed-window limit=3 window=1m0s")
	for i := 1; i <= 5; i++ {
		d, err := lim.Allow(ctx, "user:42")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("request %d: allowed=%v remaining=%d\n", i, d.Allowed, d.Remaining)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fixed-window limit=3 window=1m0s
request 1: allowed=true remaining=2
request 2: allowed=true remaining=1
request 3: allowed=true remaining=0
request 4: allowed=false remaining=0
request 5: allowed=false remaining=0
```

### Tests

The tests use `miniredis`, which implements `EVAL`, sorted sets, and a clock you
control. `FastForward` decreases every TTL (so a fixed-window key expires),
while `SetTime` moves the clock that `redis.call('TIME')` reads inside the script
(so a sliding window ages out its entries). `TestFixedWindowBoundaryBurst`
demonstrates the fixed-window flaw — a fresh full quota immediately after the
previous window — that `TestSlidingWindowRejectsBoundaryBurst` shows the sliding
window rejecting. `TestConcurrentAllow` fires goroutines at one key and asserts
the admitted total never exceeds the limit, which is the atomicity proof.

Create `limiter_test.go`:

```go
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestFixedWindowLimit(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestClient(t)
	lim := NewFixedWindow(rdb, 5, time.Minute)
	ctx := context.Background()

	allowed := 0
	for i := range 8 {
		d, err := lim.Allow(ctx, "k")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("allowed = %d; want 5", allowed)
	}
}

func TestFixedWindowReset(t *testing.T) {
	t.Parallel()
	rdb, mr := newTestClient(t)
	lim := NewFixedWindow(rdb, 2, time.Minute)
	ctx := context.Background()

	for range 2 {
		if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("request within limit was denied")
		}
	}
	if d, _ := lim.Allow(ctx, "k"); d.Allowed {
		t.Fatal("request over limit was allowed")
	}

	mr.FastForward(time.Minute) // expire the window key
	if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
		t.Fatal("quota did not reset after the window expired")
	}
}

func TestFixedWindowBoundaryBurst(t *testing.T) {
	t.Parallel()
	rdb, mr := newTestClient(t)
	lim := NewFixedWindow(rdb, 3, time.Minute)
	ctx := context.Background()

	// Drain the quota at the end of one window.
	for range 3 {
		if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("request within limit denied")
		}
	}
	// Cross the boundary: a fresh full quota is available immediately, so 6
	// requests pass in a span around the boundary shorter than one window.
	mr.FastForward(time.Minute)
	burst := 0
	for range 3 {
		if d, _ := lim.Allow(ctx, "k"); d.Allowed {
			burst++
		}
	}
	if burst != 3 {
		t.Fatalf("post-boundary burst = %d; want 3 (fixed-window flaw)", burst)
	}
}

func TestSlidingWindowRejectsBoundaryBurst(t *testing.T) {
	t.Parallel()
	rdb, mr := newTestClient(t)
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mr.SetTime(base)
	lim := NewSlidingWindow(rdb, 3, time.Minute)
	ctx := context.Background()

	for range 3 {
		if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("request within limit denied")
		}
	}
	// Move to just before the window edge: the earlier entries are still inside
	// the rolling window, so the sliding limiter rejects the burst.
	mr.SetTime(base.Add(time.Minute - time.Millisecond))
	if d, _ := lim.Allow(ctx, "k"); d.Allowed {
		t.Fatal("sliding window admitted a boundary burst")
	} else if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v; want positive on denial", d.RetryAfter)
	}
}

func TestSlidingWindowReset(t *testing.T) {
	t.Parallel()
	rdb, mr := newTestClient(t)
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mr.SetTime(base)
	lim := NewSlidingWindow(rdb, 2, time.Minute)
	ctx := context.Background()

	for range 2 {
		if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
			t.Fatal("request within limit denied")
		}
	}
	if d, _ := lim.Allow(ctx, "k"); d.Allowed {
		t.Fatal("request over limit allowed")
	}

	mr.SetTime(base.Add(time.Minute + time.Second)) // all entries age out
	if d, _ := lim.Allow(ctx, "k"); !d.Allowed {
		t.Fatal("quota did not reset after the window rolled past")
	}
}

func TestConcurrentAllow(t *testing.T) {
	t.Parallel()
	rdb, _ := newTestClient(t)
	lim := NewFixedWindow(rdb, 10, time.Minute)
	ctx := context.Background()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := lim.Allow(ctx, "hot"); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed = %d; want exactly 10 (atomicity)", got)
	}
}

func Example() {
	mr, _ := miniredis.Run()
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	lim := NewFixedWindow(rdb, 2, time.Minute)
	for i := 1; i <= 3; i++ {
		d, _ := lim.Allow(context.Background(), "user:1")
		fmt.Printf("req %d allowed=%v remaining=%d\n", i, d.Allowed, d.Remaining)
	}
	// Output:
	// req 1 allowed=true remaining=1
	// req 2 allowed=true remaining=0
	// req 3 allowed=false remaining=0
}
```

The integration test reuses the same expectations against a real Redis. It is
tagged `integration` so the offline `go test ./...` never touches the network;
run it explicitly with `-tags integration` and a `REDIS_ADDR` pointing at a live
server.

Create `limiter_integration_test.go`:

```go
//go:build integration

package ratelimit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestFixedWindowLimitRedis(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	key := "test:rl:" + t.Name()
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}

	lim := NewFixedWindow(rdb, 3, time.Minute)
	allowed := 0
	for range 5 {
		d, err := lim.Allow(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("allowed = %d; want 3", allowed)
	}
}
```

## Review

The limiter is correct when the count-decide-record is genuinely one server-side
step. The proof is `TestConcurrentAllow`: fifty goroutines race the same key with
a limit of ten and exactly ten are admitted; if `Allow` had done `GET` then
`INCR` in Go, the count would exceed ten under the race. The common structural
mistakes are all here to avoid. Do not split the script into separate commands
"for readability" — that reintroduces the race. Do not stamp sliding-window
entries with `time.Now()` in Go; read `redis.call('TIME')` inside the script so a
skewed client cannot corrupt its window. Do not forget the `PEXPIRE` inside each
script; without it every distinct key leaks Redis memory forever. And remember the
member must be unique — the score is the clock, the member is just an
uncolliding tag.

Confirm correctness by running `go test -race ./...`: the reset tests prove the
TTL and the rolling window both reclaim quota, and the two boundary tests prove
the algorithm difference — fixed-window admits the 2x boundary burst, sliding
window rejects it. For a real Redis, run `go test -tags integration -race ./...`
with `REDIS_ADDR` set.

## Resources

- [Redis EVAL and Lua scripting](https://redis.io/docs/latest/develop/programmability/eval-intro/) — why a script runs atomically and how `KEYS`/`ARGV` work.
- [Redis rate-limiting patterns](https://redis.io/tutorials/howtos/ratelimiting/) — fixed-window, sliding-window, and token-bucket implementations.
- [go-redis scripting](https://pkg.go.dev/github.com/redis/go-redis/v9#Script) — `NewScript` and `(*Script).Run` (EVALSHA with EVAL fallback).
- [alicebob/miniredis](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — in-process Redis with `EVAL`, sorted sets, `FastForward`, and `SetTime`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-token-bucket-gcra-redis-rate.md](02-token-bucket-gcra-redis-rate.md)
