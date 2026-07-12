# Exercise 12: Request Hedging: Sizing the Reply Buffer to the Number of Senders to Kill Loser Leaks

**Level: Advanced**

To shave tail latency a client fires the same read at N replicas and takes the first
successful answer, cancelling the rest. The trap is what happens to the losers: each
one still finishes its in-flight call and tries to hand its result back, and if the
reply channel is unbuffered (or under-sized) that send has no receiver left — every
loser blocks forever on the send and leaks a goroutine. The fix is the canonical
buffered-channel sizing decision: give the reply channel `cap == number of senders`
so a losing send always has a slot and the goroutine exits. This exercise builds
`Hedge` and proves first-wins selection, cancellation of the losers, aggregated
failure via `errors.Join`, and verified leak-freedom under goleak.

This module is self-contained: its own module, a `hedge` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
hedge/                       independent module: example.com/hedge
  go.mod                     go 1.26, require go.uber.org/goleak
  hedge.go                   Hedge(ctx, replicas, call) (Response, error) with a cap==replicas reply channel
  cmd/demo/main.go           runnable demo: first-wins read, then an all-fail errors.Join
  hedge_test.go              first-wins + cancel losers, errors.Join covers each cause, parent-cancel observed, no-leak
```

- Files: `hedge.go`, `cmd/demo/main.go`, `hedge_test.go`.
- Implement: `type Response struct { Replica int; Body string }` and `Hedge(ctx context.Context, replicas int, call func(ctx context.Context, replica int) (string, error)) (Response, error)` using a reply channel of `cap == replicas` and a cancellable derived context.
- Test: the first success is returned and the losers are cancelled; a total failure returns `errors.Join` that satisfies `errors.Is` for every cause; a cancelled parent is observed as `ctx.Done()` inside `call`; a loser that sends *after* Hedge returns still exits (goleak).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### Why the reply buffer must equal the number of senders

Hedging is a fan-out of N identical calls where you consume exactly one result and
discard N-1. The discarded goroutines are the whole problem. A losing replica does not
stop the instant a peer wins — its RPC is already in flight, the socket read is already
parked in the kernel. Cancelling the context tells it to *stop soon*, but "soon" can be
after `Hedge` has already returned to its caller. When that loser finally returns from
its call and executes `replies <- result`, who receives it? Nobody. `Hedge` took the
one value it wanted and left. On an unbuffered channel that send is a rendezvous with a
receiver that will never arrive, so the goroutine blocks on `chan send` for the life of
the process. Multiply by every hedged request the service ever makes and you have a
goroutine leak that looks like a slow memory climb in production.

The sizing rule that fixes it: **a reply channel that N goroutines send to exactly once,
and whose receiver may stop reading early, must have `cap == N`.** With `cap == replicas`
every sender owns a reserved slot up front. The total number of sends is bounded by
`replicas`, so even in the worst case — `Hedge` returns after receiving zero of them —
all N sends fit in the buffer without a receiver. No send ever blocks; every goroutine
runs to its `return` and exits. This is the buffered channel used precisely for what it
is good at: decoupling a sender from a receiver that is no longer there. It is *not*
oversizing "to be safe" — the capacity is derived from the exact count of senders, which
is the only defensible reason to pick a non-zero buffer.

The protocol inside `Hedge`:

1. Reject `replicas < 1`, then derive a cancellable context with `context.WithCancel`
   and `defer cancel()` so returning cancels every still-running loser.
2. Allocate `replies := make(chan reply, replicas)` — the load-bearing cap.
3. Launch `replicas` goroutines; each runs `call(ctx, i)` and sends its outcome into
   `replies`. The send never blocks because of the buffer.
4. Receive up to `replicas` results. On the first `err == nil`, call `cancel()` (wake
   the losers) and return that `Response`.
5. If all `replicas` results are errors, return `errors.Join` of them so the caller can
   `errors.Is` every underlying cause.

Create `hedge.go`:

```go
package hedge

import (
	"context"
	"errors"
)

// Response is the outcome of the winning replica.
type Response struct {
	Replica int
	Body    string
}

// reply carries one replica's outcome back to Hedge over the reply channel.
type reply struct {
	replica int
	body    string
	err     error
}

// Hedge dispatches the same read to `replicas` goroutines, returns the first
// successful Response, and cancels the losers. If every replica fails it returns
// errors.Join of every replica error.
//
// The reply channel has cap == replicas: one reserved slot per sender. That is the
// load-bearing sizing decision. A losing goroutine finishes its in-flight call after
// Hedge has already returned and the winner's slot has been consumed, yet its send
// still fits in the buffer and never blocks, so the goroutine exits instead of
// leaking. An unbuffered (or under-sized) reply channel would block every loser's
// send forever.
func Hedge(
	ctx context.Context,
	replicas int,
	call func(ctx context.Context, replica int) (string, error),
) (Response, error) {
	if replicas < 1 {
		return Response{}, errors.New("hedge: replicas must be >= 1")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // returning cancels every still-running loser

	// cap == replicas: every sender has a reserved slot, so no send ever blocks,
	// even the losers that send after Hedge has returned.
	replies := make(chan reply, replicas)

	for i := range replicas {
		go func() {
			body, err := call(ctx, i)
			replies <- reply{replica: i, body: body, err: err}
		}()
	}

	errs := make([]error, 0, replicas)
	for range replicas {
		r := <-replies
		if r.err == nil {
			cancel() // first success: propagate cancellation to the losers
			return Response{Replica: r.replica, Body: r.body}, nil
		}
		errs = append(errs, r.err)
	}
	return Response{}, errors.Join(errs...)
}
```

### The runnable demo

The demo runs two scenarios. First a hedged read where replica 1 answers immediately
while the peers park on the context — first success wins deterministically. Then an
all-fail case where every replica errors and `Hedge` returns `errors.Join`; the demo
probes the join with `errors.Is` per sentinel so its output does not depend on the
nondeterministic arrival order of the failures.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/hedge"
)

func main() {
	// Scenario 1: replica 1 answers immediately; replicas 0 and 2 wait on the
	// context and bow out once the winner cancels them. First success wins, and it
	// wins deterministically because only replica 1 can produce a reply before the
	// cancellation fires.
	fast := func(ctx context.Context, replica int) (string, error) {
		if replica == 1 {
			return "payload-from-replica-1", nil
		}
		<-ctx.Done() // parked until the winner's success cancels the derived context
		return "", ctx.Err()
	}
	resp, err := hedge.Hedge(context.Background(), 3, fast)
	fmt.Printf("hedged read: replica=%d body=%q err=%v\n", resp.Replica, resp.Body, err)

	// Scenario 2: every replica fails; Hedge returns errors.Join of all three. We
	// probe the join with errors.Is per sentinel so the output does not depend on
	// the (nondeterministic) arrival order of the failures.
	errs := []error{
		errors.New("replica 0: connection refused"),
		errors.New("replica 1: timeout"),
		errors.New("replica 2: 503 unavailable"),
	}
	allFail := func(_ context.Context, replica int) (string, error) {
		return "", errs[replica]
	}
	_, err = hedge.Hedge(context.Background(), 3, allFail)
	fmt.Println("all replicas failed; errors.Join covers each cause:")
	for _, e := range errs {
		fmt.Printf("  errors.Is(err, %q) = %t\n", e, errors.Is(err, e))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hedged read: replica=1 body="payload-from-replica-1" err=<nil>
all replicas failed; errors.Join covers each cause:
  errors.Is(err, "replica 0: connection refused") = true
  errors.Is(err, "replica 1: timeout") = true
  errors.Is(err, "replica 2: 503 unavailable") = true
```

### Tests

`TestMain` installs `goleak.VerifyTestMain`, the package-wide proof that no goroutine
outlives the tests. `TestFirstSuccessWinsAndCancelsLosers` makes replica 1 the only one
that can reply before cancellation, asserts the returned `Response` is that fast one, and
then uses two blocking reads on a buffered channel to synchronize on both losers reaching
`ctx.Done()` — evidence of cancellation propagation, with no sleeps.
`TestAllFailJoinsEveryError` gives each replica a distinct sentinel and asserts the
returned error satisfies `errors.Is` for every one, proving the `errors.Join` aggregation.
`TestParentCancelObservedInCall` cancels the *parent* before `Hedge` derives its child and
asserts all three replicas observe `ctx.Done()` and report `context.Canceled`.
`TestLoserSendAfterReturnDoesNotLeak` is the load-bearing test: replica 0 wins
immediately, then the test releases the losers to attempt their reply send only *after*
`Hedge` has returned, and `goleak.VerifyNone` confirms they still exit — which is
impossible with an unbuffered reply channel, where those sends would block forever.

Create `hedge_test.go`:

```go
package hedge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.uber.org/goleak"
)

// VerifyTestMain fails the package if any goroutine outlives the tests. It is the
// global proof that the losing replicas exit rather than leaking on their reply send.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestFirstSuccessWinsAndCancelsLosers pins first-wins selection and cancellation
// propagation: one fast replica returns immediately while the peers park on the
// context, so the fast Response is returned and the losers observe cancellation.
func TestFirstSuccessWinsAndCancelsLosers(t *testing.T) {
	loserCancelled := make(chan int, 3) // buffered: loser sends never block

	call := func(ctx context.Context, replica int) (string, error) {
		if replica == 1 {
			return "fast", nil // the only reply that can arrive before cancellation
		}
		<-ctx.Done() // released only when the winner cancels the derived context
		loserCancelled <- replica
		return "", ctx.Err()
	}

	resp, err := Hedge(context.Background(), 3, call)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.Replica != 1 || resp.Body != "fast" {
		t.Fatalf("resp = %+v, want {Replica:1 Body:fast}", resp)
	}

	// Blocking reads synchronize on both losers reaching ctx.Done(): no sleeps.
	got := []int{<-loserCancelled, <-loserCancelled}
	slices.Sort(got)
	if want := []int{0, 2}; !slices.Equal(got, want) {
		t.Fatalf("cancelled losers = %v, want %v", got, want)
	}
}

// TestAllFailJoinsEveryError pins the aggregated-failure contract: when every replica
// errors, the returned error satisfies errors.Is for each underlying cause via
// errors.Join.
func TestAllFailJoinsEveryError(t *testing.T) {
	errs := []error{
		errors.New("replica 0 down"),
		errors.New("replica 1 down"),
		errors.New("replica 2 down"),
	}
	call := func(_ context.Context, replica int) (string, error) {
		return "", errs[replica]
	}

	resp, err := Hedge(context.Background(), 3, call)
	if resp != (Response{}) {
		t.Fatalf("resp = %+v, want zero Response on total failure", resp)
	}
	for i, e := range errs {
		if !errors.Is(err, e) {
			t.Fatalf("errors.Is(err, errs[%d]) = false; want the join to cover every cause (err = %v)", i, err)
		}
	}
}

// TestParentCancelObservedInCall proves the derived context forwards the parent's
// cancellation: with the parent already cancelled, every replica observes ctx.Done()
// and reports context.Canceled, which the join surfaces.
func TestParentCancelObservedInCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel the parent before Hedge derives its child

	observed := make(chan int, 3) // buffered so the observation send never blocks
	call := func(ctx context.Context, replica int) (string, error) {
		<-ctx.Done() // must fire because the parent is already cancelled
		observed <- replica
		return "", ctx.Err()
	}

	_, err := Hedge(ctx, 3, call)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(context.Canceled)", err)
	}
	seen := []int{<-observed, <-observed, <-observed}
	slices.Sort(seen)
	if want := []int{0, 1, 2}; !slices.Equal(seen, want) {
		t.Fatalf("replicas observing ctx.Done() = %v, want %v", seen, want)
	}
}

// TestLoserSendAfterReturnDoesNotLeak is the load-bearing test. It releases the losing
// replicas to attempt their reply send only AFTER Hedge has already returned and the
// winner's slot has been consumed. Because the reply channel has cap == replicas, each
// loser's send still fits and the goroutine exits; goleak.VerifyNone confirms it. With
// an unbuffered reply channel the loser sends would block forever and this would fail.
func TestLoserSendAfterReturnDoesNotLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	release := make(chan struct{})
	call := func(_ context.Context, replica int) (string, error) {
		if replica == 0 {
			return "winner", nil // returns immediately; wins deterministically
		}
		<-release // parked until the test releases us, which is AFTER Hedge returns
		return "loser", nil
	}

	resp, err := Hedge(context.Background(), 4, call)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if resp.Replica != 0 || resp.Body != "winner" {
		t.Fatalf("resp = %+v, want {Replica:0 Body:winner}", resp)
	}

	// Hedge has returned. The three loser goroutines are parked inside call. Release
	// them: each returns, its goroutine sends into the buffered reply channel without
	// blocking, and exits. goleak.VerifyNone polls until every goroutine is gone.
	close(release)
}
```

## Review

Correct here means four things at once: the first successful reply is what `Hedge`
returns (proven by making one replica the only one able to answer before cancellation),
the losers are actually cancelled (proven by blocking on their observation of
`ctx.Done()`), a total failure surfaces every cause (proven by `errors.Is` against each
sentinel in the `errors.Join`), and — the property that motivates the whole exercise —
no losing goroutine leaks. The single invariant that guarantees leak-freedom is the reply
channel's `cap == replicas`: because at most `replicas` sends ever happen and each has a
reserved slot, a loser that returns *after* `Hedge` has taken its winner and left still
lands its send in the buffer and exits. The leak test proves it by releasing the losers
only after `Hedge` returns and letting `goleak.VerifyNone` confirm they are gone; drop the
buffer to unbuffered and those same sends park on `chan send` forever. This is the exact
production bug behind slow goroutine growth in hedged, timeout, and race-the-replicas
clients: the winner is handled, the losers are forgotten, and an under-sized reply channel
turns "forgotten" into "leaked."

## Resources

- [The Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the receiver-stops-early leak this buffer sizing prevents.
- [pkg.go.dev: context](https://pkg.go.dev/context) — `WithCancel` and the `Done()` propagation the losers observe.
- [pkg.go.dev: errors#Join](https://pkg.go.dev/errors#Join) — aggregating replica failures so `errors.Is` matches each cause.
- [pkg.go.dev: go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain` and `VerifyNone`, the goroutine-leak detectors this test relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-state-push-conflation-cap-one-mailbox.md](11-state-push-conflation-cap-one-mailbox.md) | Next: [13-multipart-upload-bounded-inflight-parts.md](13-multipart-upload-bounded-inflight-parts.md)
