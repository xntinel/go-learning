# Exercise 2: Combine A Server Request's Cancellation Sources

A real request handler has more than one reason to stop: the process is shutting down, the request's own deadline elapsed, or an upstream dependency went fatally unhealthy. This exercise folds those named sources into a single done channel a handler can wait on with one `select` arm — and, unlike a bare or-channel, it records which source fired so the handler can return the right status.

This module is fully self-contained. It begins with its own `go mod init`, defines everything it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
shutdown.go          Source, Combine (cause-tracking or-channel), After, FromContext, FromChannel
cmd/
  demo/
    main.go          combine shutdown + deadline + upstream, fire one, report the cause
shutdown_test.go     each source wins, race close-once, zero sources never fire, cause accessor
```

- Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
- Implement: `Combine(sources ...Source) (done <-chan struct{}, cause func() string)` plus the `Source` type and the `After`, `FromContext`, and `FromChannel` constructors.
- Test: each source can win and is named correctly, many sources firing at once close the done channel exactly once, zero sources never fire, and `cause` reports the empty string until a source fires.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p combine-cancellation/cmd/demo && cd combine-cancellation
go mod init example.com/combine-cancellation
```

### Why a cause-tracking combiner, not a bare or-channel

A bare or-channel tells a handler that it should stop, but not why. In a server that distinction is the difference between a `503 Service Unavailable` (shutting down), a `504 Gateway Timeout` (deadline elapsed), and a `502 Bad Gateway` (upstream failed). So this combiner is an or-channel with one addition: it remembers the name of the first source that fired and exposes it through a `cause` accessor.

A `Source` is a named `<-chan struct{}`. `Combine` spawns one goroutine per source, each selecting on its source's channel and on the shared done channel — the same two-arm structure as the core or-channel, so losers exit cleanly and never leak. The first source to fire runs a `fire` helper that, under a `sync.Once`, records the cause and then closes the done channel. The ordering inside `once.Do` matters: the cause is written before the channel is closed, so any goroutine that observes the closed channel and then calls `cause()` is guaranteed by the channel-close happens-before edge to see the recorded name, not the empty string. The `cause` accessor takes the same mutex the writer holds, so a reader that races the writer before the channel closes still reads safely rather than tearing the string under the race detector.

The constructors turn the three real sources into uniform `Source` values. `FromChannel` wraps a channel you already hold, such as a server-wide shutdown channel closed on SIGTERM. `FromContext` adapts a `context.Context`, so a request's existing deadline or parent cancellation becomes a source by handing over `ctx.Done()`. `After` builds a deadline source from a duration, closing its channel when the timer fires; it is the per-request timeout expressed as an or-channel input.

The zero-sources case takes the opposite convention from the classic or-channel, and on purpose: a request with no cancellation sources is one that is never cancelled, so `Combine()` returns a done channel that never closes and a `cause` that stays empty. The domain, not dogma, picks the boundary behavior, and the test pins this choice.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"sync"
	"time"
)

// Source is a named cancellation signal. Done closes when the reason it
// represents occurs. A nil Done is ignored (that reason can never fire).
type Source struct {
	Name string
	Done <-chan struct{}
}

// Combine folds the sources into one done channel that closes when the first
// source fires. The returned cause function reports the name of that first
// source, or the empty string until one fires. Combine() with no sources
// returns a channel that never closes.
func Combine(sources ...Source) (<-chan struct{}, func() string) {
	done := make(chan struct{})
	var (
		once  sync.Once
		mu    sync.Mutex
		cause string
	)

	fire := func(name string) {
		once.Do(func() {
			mu.Lock()
			cause = name
			mu.Unlock()
			close(done)
		})
	}

	for _, s := range sources {
		if s.Done == nil {
			continue
		}
		s := s
		go func() {
			select {
			case <-s.Done:
				fire(s.Name)
			case <-done:
				// Another source already won; exit cleanly.
			}
		}()
	}

	get := func() string {
		mu.Lock()
		defer mu.Unlock()
		return cause
	}
	return done, get
}

// FromChannel wraps an existing done channel, such as a server-wide shutdown
// channel closed on SIGTERM.
func FromChannel(name string, ch <-chan struct{}) Source {
	return Source{Name: name, Done: ch}
}

// FromContext adapts a context's cancellation into a Source.
func FromContext(name string, ctx context.Context) Source {
	return Source{Name: name, Done: ctx.Done()}
}

// After builds a deadline source whose channel closes after d elapses.
func After(name string, d time.Duration) Source {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return Source{Name: name, Done: ch}
}
```

The `fire` helper is the heart of the file. Writing `cause` under `mu` and only then calling `close(done)` establishes the order a reader depends on; closing first would let a reader see the closed channel and still read an empty cause. Each goroutine captures its own `s` and selects on both arms, so exactly one source wins and the rest return when `done` closes.

### The runnable demo

The demo wires the three real sources of a request: a shutdown channel that stays open, a 40 ms deadline, and an upstream watcher that reports failure at 20 ms. The upstream fires first, so the handler learns its cause is `upstream` and would return a `502`. The cause is deterministic because the upstream deadline is strictly shorter than the request deadline.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/combine-cancellation"
)

func main() {
	serverShutdown := make(chan struct{}) // stays open in this run.
	upstreamFailed := make(chan struct{})
	go func() { time.Sleep(20 * time.Millisecond); close(upstreamFailed) }()

	done, cause := shutdown.Combine(
		shutdown.FromChannel("shutdown", serverShutdown),
		shutdown.After("deadline", 40*time.Millisecond),
		shutdown.FromChannel("upstream", upstreamFailed),
	)

	<-done
	fmt.Printf("request cancelled by: %s\n", cause())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request cancelled by: upstream
```

### Tests

The suite drives each source to victory in isolation, checks the race behavior, and pins the boundary cases. `TestEachSourceCanWin` fires the shutdown, deadline, and upstream sources one at a time and asserts the cause is named correctly each time. `TestCombineCloseOnceUnderRace` fires many sources simultaneously and asserts the done channel closes exactly once with a non-empty cause and no panic. `TestZeroSourcesNeverFires` asserts `Combine()` does not close within the window. `TestCauseEmptyUntilFire` asserts the accessor returns the empty string before anything fires.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestEachSourceCanWin(t *testing.T) {
	t.Parallel()

	cases := []string{"shutdown", "deadline", "upstream"}
	for _, winner := range cases {
		winner := winner
		t.Run(winner, func(t *testing.T) {
			t.Parallel()

			fire := make(chan struct{})
			never1 := make(chan struct{})
			never2 := make(chan struct{})

			sources := make([]Source, 3)
			for i, name := range cases {
				switch {
				case name == winner:
					sources[i] = FromChannel(name, fire)
				case i == 0:
					sources[i] = FromChannel(name, never1)
				default:
					sources[i] = FromChannel(name, never2)
				}
			}

			done, cause := Combine(sources...)
			close(fire)

			select {
			case <-done:
			case <-time.After(50 * time.Millisecond):
				t.Fatal("Combine did not close when a source fired")
			}
			if got := cause(); got != winner {
				t.Fatalf("cause = %q, want %q", got, winner)
			}
		})
	}
}

func TestCombineCloseOnceUnderRace(t *testing.T) {
	t.Parallel()

	const n = 32
	chs := make([]chan struct{}, n)
	sources := make([]Source, n)
	for i := range chs {
		chs[i] = make(chan struct{})
		sources[i] = FromChannel("s", chs[i])
	}

	done, cause := Combine(sources...)

	var wg sync.WaitGroup
	for _, c := range chs {
		wg.Add(1)
		go func(c chan struct{}) {
			defer wg.Done()
			close(c)
		}(c)
	}
	wg.Wait()

	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Combine did not close")
	}
	if cause() == "" {
		t.Fatal("cause was empty after a source fired")
	}
}

func TestZeroSourcesNeverFires(t *testing.T) {
	t.Parallel()

	done, cause := Combine()
	select {
	case <-done:
		t.Fatal("Combine() with no sources closed; it must never fire")
	case <-time.After(30 * time.Millisecond):
	}
	if cause() != "" {
		t.Fatalf("cause = %q, want empty", cause())
	}
}

func TestCauseEmptyUntilFire(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, cause := Combine(FromContext("ctx", ctx))
	if got := cause(); got != "" {
		t.Fatalf("cause before fire = %q, want empty", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Combine did not close when the context was cancelled")
	}
	if got := cause(); got != "ctx" {
		t.Fatalf("cause = %q, want %q", got, "ctx")
	}
}
```

## Review

The combiner is correct when exactly one cause is recorded, the done channel closes exactly once, and a reader never tears the cause string. The `sync.Once` guarantees the single close and the single cause write; the mutex around both the write and the `cause` accessor keeps the race detector clean even when a reader races the writer before the channel closes. The ordering inside `once.Do` — write the cause, then close the channel — is what lets a goroutine that observed the close trust `cause()` to be populated; reversing those two lines reintroduces a window where the channel is closed but the cause is still empty.

The mistakes to avoid mirror the core pattern plus one of its own. Closing the done channel from outside the `sync.Once`, or writing the cause after the close, breaks the exactly-once-and-ordered contract. Forgetting the `<-done` arm in each goroutine leaks the losers. And picking the wrong zero-sources convention silently changes behavior: here, no sources means never cancel, which is the safe server default and is pinned by `TestZeroSourcesNeverFires`. Run `go test -race ./...`; the race test firing 32 sources at once is the one that proves the close-once and cause-write are airtight.

## Resources

- [`context`](https://pkg.go.dev/context) — the standard package whose `Done()` channel `FromContext` adapts, and the model server handlers already use for deadlines and cancellation.
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) — how a server propagates cancellation and deadlines across a request's goroutines.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — the timer that `After` uses to turn a per-request deadline into a cancellation source.

---

Back to [01-or-channel-core.md](01-or-channel-core.md) | Next: [03-first-of-n-responder.md](03-first-of-n-responder.md)
</content>
