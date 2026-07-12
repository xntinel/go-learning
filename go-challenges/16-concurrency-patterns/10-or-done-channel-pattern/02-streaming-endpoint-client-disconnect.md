# Exercise 2: A Streaming Endpoint That Stops on Client Disconnect

A real server-sent-events endpoint forwards an upstream feed to the client for
as long as the client is connected, and not one tick longer. The hard part is
not the streaming — it is the teardown. When the browser tab closes, the
upstream feed and every goroutine attached to it must stop, or the server leaks
a goroutine per dropped connection until it falls over. This exercise wires the
or-done wrapper to `r.Context().Done()`, which `net/http` cancels the instant
the client disconnects, and proves with a goroutine-count assertion that the
disconnect tears the whole chain down.

This module is fully self-contained. It starts with its own `go mod init`,
bundles its own copy of the or-done wrapper, and ships its own demo and tests.
Nothing here imports any other exercise.

## What you'll build

```text
stream.go            OrDone[T] (bundled), eventSource (ctx-bound producer),
                     StreamHandler (SSE handler ranging over OrDone)
cmd/
  demo/
    main.go          serve, stream three events to a client, disconnect, prove no leak
stream_test.go       disconnect-stops-stream + no-leak, eventSource-stops-on-cancel
```

- Files: `stream.go`, `cmd/demo/main.go`, `stream_test.go`.
- Implement: `eventSource(ctx, interval) <-chan string`, `StreamHandler(interval) http.HandlerFunc`,
  and the bundled `OrDone[T any](done <-chan struct{}, in <-chan T) <-chan T`.
- Test: a client reads several events, disconnects via context cancel, and the
  test asserts the server's goroutine count returns to its pre-request baseline.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/10-or-done-channel-pattern/02-streaming-endpoint-client-disconnect/cmd/demo && cd go-solutions/16-concurrency-patterns/10-or-done-channel-pattern/02-streaming-endpoint-client-disconnect
```

### Why the request context is the right done channel

A streaming handler has no natural end: the upstream feed runs forever and the
handler returns only when someone tells it to stop. The one authority that knows
the client is gone is the `net/http` server. Since Go 1.8 the server attaches a
context to every request and cancels it when the underlying connection closes;
`r.Context().Done()` is the channel that fires on disconnect. Because that
channel is exactly the `<-chan struct{}` the or-done wrapper accepts, the
handler becomes disconnect-aware with no extra plumbing: `OrDone(r.Context().Done(), feed)`
forwards events while the client is connected and stops the moment it is not.

The leak that this prevents is subtle because the obvious half is easy. Cancel
the wrapper and its forwarding goroutine exits — fine. But the upstream feed is
a goroutine too, and if it is a plain `for { out <- next() }` it blocks forever
on a send into a channel the cancelled wrapper has stopped reading. The handler
returned, the connection is closed, and yet a producer goroutine lives on. So
the feed must watch the same context: `eventSource` takes `ctx` and selects on
`ctx.Done()` both while waiting for the next tick and while sending, so it
returns when the request is cancelled. The discipline is the one from the
concepts file — cancel the whole chain, not just the wrapper — applied to an
HTTP request whose lifetime is the cancellation signal.

Dropping the in-flight event on disconnect is correct here. A client that just
closed its connection cannot receive anything, so the at-most-one event the
wrapper discards on cancel is an event that had nowhere to go. This is exactly
the kind of stream the dropping wrapper is built for, which is why this exercise
uses it and the next one does not.

Create `stream.go`:

```go
// Package stream serves an upstream event feed over HTTP and stops cleanly,
// with no goroutine leak, the instant the client disconnects.
package stream

import (
	"fmt"
	"net/http"
	"time"
)

// OrDone forwards values from in to the returned channel until in closes or done
// fires, then closes the returned channel. The caller owns done; OrDone owns and
// closes the output. An in-flight value may be dropped when done fires.
func OrDone[T any](done <-chan struct{}, in <-chan T) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- v:
				case <-done:
					return
				}
			}
		}
	}()
	return out
}

// eventSource emits "event N" strings every interval until ctx is cancelled.
// It owns its goroutine and closes the returned channel on exit, and it selects
// on ctx.Done() both while waiting and while sending so it never leaks when the
// consumer goes away.
func eventSource(ctx context.Context, interval time.Duration) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		t := time.NewTicker(interval)
		defer t.Stop()
		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case out <- fmt.Sprintf("event %d", n):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// StreamHandler returns an SSE handler that forwards eventSource to the client
// until the client disconnects. The request context, cancelled by net/http on
// disconnect, is the done channel passed to OrDone, so the handler and the feed
// both stop when the connection closes.
func StreamHandler(interval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		ctx := r.Context()
		feed := eventSource(ctx, interval)
		for msg := range OrDone(ctx.Done(), feed) {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
```

The `context` import is needed by `eventSource`; add it to the import block:

```go
package stream

import (
	"context"
	"fmt"
	"net/http"
	"time"
)
```

Trace the disconnect. The client closes its connection; the `net/http` server's
background read on that connection sees the close and cancels `r.Context()`.
That closes `ctx.Done()`, which the `OrDone` goroutine is selecting on, so it
returns and closes its output. The handler's `for ... range` ends and the
handler returns. The same closed `ctx.Done()` is selected on by `eventSource`,
so its goroutine returns and closes `feed`. Three goroutines — handler, wrapper,
feed — all unwound from one signal, which is what the test verifies by counting.

### The runnable demo

The demo runs the whole loop in one process: it starts a server, connects a
client, reads three events, disconnects, and confirms the server's goroutine
count returns to where it started. The output is fixed because the event numbers
are deterministic and the leak check is a boolean.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"time"

	"example.com/stream"
)

func main() {
	ts := httptest.NewServer(stream.StreamHandler(5 * time.Millisecond))
	defer ts.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()

	// Let the server settle, then snapshot the goroutine count.
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("request error:", err)
		cancel()
		return
	}

	sc := bufio.NewScanner(resp.Body)
	read := 0
	for sc.Scan() && read < 3 {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			fmt.Println(line)
			read++
		}
	}

	// Disconnect.
	cancel()
	resp.Body.Close()

	leaked := true
	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= base {
			leaked = false
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("client disconnected; no goroutine leak:", !leaked)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
data: event 1
data: event 2
data: event 3
client disconnected; no goroutine leak: true
```

### Tests

The tests assert both halves of correct teardown. `TestEventSourceStopsOnCancel`
checks the producer in isolation: cancelling its context closes its channel and
its goroutine returns to baseline. `TestDisconnectStopsStreamNoLeak` is the
integration case: a client reads several events over a real `httptest` server,
then disconnects via context cancel, and the test polls until the goroutine
count returns to the pre-request baseline — if a handler, wrapper, or feed
goroutine leaked, the count never settles and the test fails.

Create `stream_test.go`:

```go
package stream

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitGoroutines polls until the goroutine count drops to at most want, or fails.
// Goroutines unwind asynchronously after cancellation, so a correct leak check
// waits for the count to settle rather than reading it once.
func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if runtime.NumGoroutine() <= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: have %d, want <= %d", runtime.NumGoroutine(), want)
}

func TestEventSourceStopsOnCancel(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	out := eventSource(ctx, time.Millisecond)

	// Read a couple of events to prove the source is live.
	for i := 0; i < 2; i++ {
		select {
		case _, ok := <-out:
			if !ok {
				t.Fatal("source closed before producing events")
			}
		case <-time.After(time.Second):
			t.Fatal("source produced no events")
		}
	}

	cancel()

	// The source must close its channel after cancel.
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				waitGoroutines(t, base)
				return
			}
		case <-deadline:
			t.Fatal("source did not close after cancel")
		}
	}
}

func TestDisconnectStopsStreamNoLeak(t *testing.T) {
	ts := httptest.NewServer(StreamHandler(2 * time.Millisecond))
	defer ts.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()

	// Snapshot after the server is up and idle.
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("request failed: %v", err)
	}

	sc := bufio.NewScanner(resp.Body)
	read := 0
	for sc.Scan() && read < 3 {
		if strings.HasPrefix(sc.Text(), "data: ") {
			read++
		}
	}
	if read < 3 {
		cancel()
		resp.Body.Close()
		t.Fatalf("received %d events, want at least 3", read)
	}

	// Disconnect and assert the whole server-side chain unwinds.
	cancel()
	resp.Body.Close()
	waitGoroutines(t, base)
}

func ExampleOrDone() {
	src := make(chan string, 2)
	src <- "a"
	src <- "b"
	close(src)

	done := make(chan struct{})
	for v := range OrDone(done, src) {
		fmt.Println(v)
	}
	// Output:
	// a
	// b
}
```

## Review

The endpoint is correct when both the wrapper and the feed watch the request
context. The most common mistake is to cancel only the wrapper: the handler
returns, the connection is closed, yet the feed goroutine blocks forever on a
send into a channel nobody reads. The cure is structural — `eventSource` selects
on `ctx.Done()` in both its waiting arm and its sending arm — and the only test
that catches its absence is the goroutine-count assertion, because a feed leak
is invisible to a test that merely checks the right bytes arrived. Run the suite
with `-race`: the detector flags any unsynchronized access between the handler
goroutine writing the response and a producer that was supposed to have stopped.

Two details are load-bearing. The leak check polls rather than reading the count
once, because cancellation only signals goroutines to exit; they unwind on their
own schedule, and a single read taken too early reports a false leak. And the
client uses `DisableKeepAlives` with `CloseIdleConnections`, so a pooled,
still-open connection does not hold a server goroutine alive and mask a real
leak with a benign one. In production this same shape is what `go.uber.org/goleak`
automates in `TestMain`; the standard-library `runtime.NumGoroutine` poll keeps
this module dependency-free.

## Resources

- [`http.Request.Context`](https://pkg.go.dev/net/http#Request.Context) — the
  per-request context that `net/http` cancels when the client connection closes.
- [`http.Flusher`](https://pkg.go.dev/net/http#Flusher) — how a handler pushes
  buffered bytes to the client between events for true streaming.
- [MDN: Server-sent events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events/Using_server-sent_events) —
  the `text/event-stream` wire format the handler emits.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — the production
  tool that automates the goroutine-leak assertion this exercise hand-rolls.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-or-done-wrapper.md](01-or-done-wrapper.md) | Next: [03-subscription-drain-unsubscribe.md](03-subscription-drain-unsubscribe.md)
</content>
