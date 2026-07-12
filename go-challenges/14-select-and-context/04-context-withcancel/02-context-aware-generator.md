# Exercise 2: Cancellable Generator (Abort a Producer Mid-Stream)

A producer goroutine that streams values on a channel must stop when the consumer
walks away — otherwise it blocks forever on an unread send and leaks. This is the
real-world shape of a paginated fetcher or a streaming row cursor: every send must
be a `select` against `ctx.Done()`, so a cancelled consumer unwinds the producer
instead of wedging it.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
generator/                 independent module: example.com/generator
  go.mod                   module example.com/generator
  generator.go             Generate(ctx, count) <-chan int (select on send vs Done)
  cmd/
    demo/
      main.go              drains a few values, cancels, watches the producer exit
  generator_test.go        full-drain, Example, and a goroutine-leak proof
```

- Files: `generator.go`, `cmd/demo/main.go`, `generator_test.go`.
- Implement: `Generate(ctx, count) <-chan int` whose producer goroutine selects on `ctx.Done()` versus the outbound send, then closes the channel when it finishes.
- Test: a full drain yields `0..count-1`; an `Example` with verified output; a cancel-after-one-read leaves no goroutine behind.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/04-context-withcancel/02-context-aware-generator/cmd/demo
cd go-solutions/14-select-and-context/04-context-withcancel/02-context-aware-generator
```

### Why the send must be a select

The naive generator writes `out <- i` directly. That is a latent leak. The
channel is unbuffered, so the send blocks until someone receives. If the consumer
reads a few values and then stops — because its own context was cancelled, or it
hit an error and returned — nobody ever receives the next value. The producer
goroutine is now parked on `out <- i` forever. It holds its stack, its captured
variables, and whatever they reference, for the life of the process. Multiply
that by every aborted request and you have a goroutine leak that shows up as slow
memory growth in production.

The fix is to make the send one arm of a `select`, with `ctx.Done()` as the
other:

```go
select {
case <-ctx.Done():
	return
case out <- i:
}
```

Now an unread send is not a deadlock: the moment the consumer's context is
cancelled, the `<-ctx.Done()` arm becomes ready, the goroutine returns, and its
stack unwinds. The consumer signals "I am done reading" by cancelling; the
producer observes it on the very send that would otherwise have blocked.

Note who owns the close. The producer closes `out` when it finishes its range —
the sender always closes, never the receiver, because only the sender knows there
are no more values coming. On the cancel path the producer `return`s *without*
closing; the consumer already stopped reading, so a close would be pointless, and
`for v := range out` on the consumer side simply stops when it stops receiving.
Leaving the channel unclosed on cancel is correct here: the channel becomes
unreachable and is collected.

Create `generator.go`:

```go
package generator

import "context"

// Generate emits the integers 0..count-1 on the returned channel, one per
// receive. The producer goroutine selects on ctx.Done() versus each send, so a
// consumer that stops reading after a cancel never wedges the producer. On a
// full drain the channel is closed; on cancel the goroutine returns and the
// channel is abandoned.
func Generate(ctx context.Context, count int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := range count {
			select {
			case <-ctx.Done():
				return
			case out <- i:
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo drains the whole stream when the context stays active, then shows the
cancel path: it captures a baseline goroutine count, reads a single value from a
fresh generator, cancels, gives the producer a moment to observe the cancel, and
then prints whether the goroutine count actually fell back to (or below) the
baseline — the boolean is computed from `runtime.NumGoroutine()`, not asserted
blindly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"example.com/generator"
)

func main() {
	full := 0
	for range generator.Generate(context.Background(), 5) {
		full++
	}
	fmt.Println("drained full stream:", full)

	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	out := generator.Generate(ctx, 1_000_000)
	fmt.Println("read one value:", <-out)
	cancel()

	// Give the producer a moment to observe the cancel and exit, then check
	// whether the goroutine count actually fell back to the pre-generator base.
	stopped := false
	for range 100 {
		if runtime.NumGoroutine() <= base {
			stopped = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Println("producer stopped after cancel:", stopped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drained full stream: 5
read one value: 0
producer stopped after cancel: true
```

### Tests

`TestGenerateCompletes` drains the whole stream and checks the values.
`ExampleGenerate` verifies the output line by line. `TestGenerateStopsWhenConsumerCancels`
is the important one: it reads a single value from a would-be-huge stream, cancels,
and then polls `runtime.NumGoroutine()` back to a baseline captured before the
generator started — proving the producer exited on the send-case `Done()` arm
rather than blocking forever on the unread send.

Create `generator_test.go`:

```go
package generator

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestGenerateCompletes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []int
	for v := range Generate(ctx, 3) {
		got = append(got, v)
	}

	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("Generate yielded %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Generate[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestGenerateStopsWhenConsumerCancels(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	out := Generate(ctx, 1_000_000)

	if v := <-out; v != 0 {
		t.Fatalf("first value = %d, want 0", v)
	}
	cancel()

	// The producer is blocked on its next send; cancel should unwedge it.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("producer goroutine leaked: NumGoroutine=%d, base=%d",
				runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}
}

func ExampleGenerate() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for v := range Generate(ctx, 3) {
		fmt.Println(v)
	}
	// Output:
	// 0
	// 1
	// 2
}
```

## Review

The generator is correct when the send is a `select` arm, not a bare `out <- i`.
The leak proof is the contract: read one value from a large stream, cancel, and
watch `runtime.NumGoroutine()` fall back to baseline — if it does not, the
producer is parked on an unread send and the `select` was missing its `Done()`
arm. Remember that on the cancel path the producer returns without closing, which
is fine; the consumer has already stopped ranging. The one thing you must not do
is have the consumer close the channel — only the sender closes, because only the
sender knows the stream is finished. Run `go test -race` to confirm the handoff
between producer and consumer is clean.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done` on the send case.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of aborting a producer with a done signal.
- [runtime.NumGoroutine](https://pkg.go.dev/runtime#NumGoroutine) — the goroutine count used in the leak proof.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-idempotent-cancel-no-leak.md](03-idempotent-cancel-no-leak.md)
