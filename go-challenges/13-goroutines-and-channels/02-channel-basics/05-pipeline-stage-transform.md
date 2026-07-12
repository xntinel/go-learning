# Exercise 5: Build a Composable Pipeline Stage that Closes Its Output

An in-process pipeline — parse, enrich, filter, batch — is built from stages that
snap together. The contract that makes them composable is small and strict: a stage
reads its input channel until it closes, then closes its own output channel,
propagating end-of-stream downstream. This exercise builds two stages that process a
stream of raw log lines and chains them, proving the close signal flows all the way
through.

This module is self-contained: its own module, a `pipeline` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                    independent module: example.com/pipeline
  go.mod                     go 1.26
  pipeline.go                type RawLog, Event; Parse, KeepLevel stages
  cmd/demo/main.go           runnable demo: parse then filter a log batch
  pipeline_test.go           transform, close-propagation, composition, empty input
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Parse(in <-chan RawLog) <-chan Event` and `KeepLevel(in <-chan Event, level string) <-chan Event`, each closing its output when its input closes.
- Test: each stage transforms every element; closing the input closes the output; two stages compose; empty input terminates cleanly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/02-channel-basics/05-pipeline-stage-transform/cmd/demo
cd go-solutions/13-goroutines-and-channels/02-channel-basics/05-pipeline-stage-transform
```

### Directional channels enforce the stage contract

Each stage has the same skeleton: `make` a new output channel, start one goroutine
that `defer close(out)`, ranges the input, transforms each value, and sends
downstream, then `return out`. The `defer close(out)` is the load-bearing line —
when `in` closes, the `for range in` loop ends, the goroutine returns, and the
deferred close fires, telling the next stage the stream is over. Miss it and the
downstream stage's range blocks forever.

The parameter type is `<-chan T` (receive-only) and the return type is `<-chan T`
as well. The receive-only input type is not decoration: it makes it a compile error
for the stage to accidentally `close(in)` or send upstream. A stage owns only the
channel it creates; it may never close a channel it was handed. Directional types
turn that ownership rule into something the compiler checks for you.

`Parse` splits a raw log line "LEVEL message..." into an `Event{Level, Msg}`.
`KeepLevel` is a filter — it forwards only events whose level matches, dropping the
rest — which is why a filter stage is where element counts change but the
close-propagation contract stays identical.

Create `pipeline.go`:

```go
package pipeline

import "strings"

// RawLog is an unparsed log line entering the pipeline.
type RawLog struct {
	Line string
}

// Event is a parsed log record.
type Event struct {
	Level string
	Msg   string
}

// Parse turns raw log lines into structured Events. It closes its output when the
// input closes. A line is "LEVEL rest of message"; a line with no space is treated
// as level-only with an empty message.
func Parse(in <-chan RawLog) <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for raw := range in {
			level, msg, _ := strings.Cut(raw.Line, " ")
			out <- Event{Level: level, Msg: msg}
		}
	}()
	return out
}

// KeepLevel forwards only the events whose Level equals level, dropping the rest.
// It closes its output when the input closes.
func KeepLevel(in <-chan Event, level string) <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for e := range in {
			if e.Level == level {
				out <- e
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo builds a source channel, feeds `Parse` into `KeepLevel`, and drains the
end of the chain — a real two-stage pipeline in a dozen lines.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pipeline"
)

func main() {
	lines := []string{
		"INFO server started",
		"ERROR db connection refused",
		"INFO request handled",
		"ERROR upstream timeout",
	}

	src := make(chan pipeline.RawLog)
	go func() {
		defer close(src)
		for _, l := range lines {
			src <- pipeline.RawLog{Line: l}
		}
	}()

	errorsOnly := pipeline.KeepLevel(pipeline.Parse(src), "ERROR")
	for e := range errorsOnly {
		fmt.Printf("%s: %s\n", e.Level, e.Msg)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ERROR: db connection refused
ERROR: upstream timeout
```

### Tests

`TestStageTransformsEachElement` feeds `Parse` and checks every input produced the
right `Event`. `TestStageClosesOutputWhenInputCloses` is the contract test: it feeds
a finite input, and asserts the output range terminates — if the stage forgot to
close, the range would hang and the test timeout would catch it.
`TestTwoStagesCompose` chains `Parse` into `KeepLevel` and asserts the filtered
result end to end. `TestEmptyInputTerminatesCleanly` sends nothing and closes the
input immediately, proving the stage emits an empty, promptly-closed output.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"fmt"
	"testing"
)

// fromLines builds a closed-when-drained source channel of raw log lines.
func fromLines(lines ...string) <-chan RawLog {
	src := make(chan RawLog)
	go func() {
		defer close(src)
		for _, l := range lines {
			src <- RawLog{Line: l}
		}
	}()
	return src
}

func fromEvents(events ...Event) <-chan Event {
	src := make(chan Event)
	go func() {
		defer close(src)
		for _, e := range events {
			src <- e
		}
	}()
	return src
}

func TestStageTransformsEachElement(t *testing.T) {
	t.Parallel()

	var got []Event
	for e := range Parse(fromLines("INFO up", "ERROR down")) {
		got = append(got, e)
	}
	want := []Event{
		{Level: "INFO", Msg: "up"},
		{Level: "ERROR", Msg: "down"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestStageClosesOutputWhenInputCloses(t *testing.T) {
	t.Parallel()

	count := 0
	for range Parse(fromLines("INFO a", "INFO b", "INFO c")) {
		count++
	}
	// If the range returns at all, the stage closed its output.
	if count != 3 {
		t.Fatalf("drained %d events, want 3", count)
	}
}

func TestTwoStagesCompose(t *testing.T) {
	t.Parallel()

	src := fromLines(
		"INFO started",
		"ERROR boom",
		"INFO ok",
		"ERROR kaboom",
	)
	var got []string
	for e := range KeepLevel(Parse(src), "ERROR") {
		got = append(got, e.Msg)
	}
	want := []string{"boom", "kaboom"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("msg %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestEmptyInputTerminatesCleanly(t *testing.T) {
	t.Parallel()

	count := 0
	for range KeepLevel(fromEvents(), "ERROR") {
		count++
	}
	if count != 0 {
		t.Fatalf("drained %d events from empty input, want 0", count)
	}
}

func ExampleKeepLevel() {
	for e := range KeepLevel(fromEvents(
		Event{Level: "INFO", Msg: "a"},
		Event{Level: "ERROR", Msg: "b"},
	), "ERROR") {
		fmt.Println(e.Msg)
	}
	// Output: b
}
```

## Review

A stage is correct when it obeys the composition contract exactly: read the input
to close, transform, and close the output — never close the input, never send
upstream. The receive-only `<-chan` parameter types make the last two violations
compile errors, so the only way to break the contract is to drop the
`defer close(out)`, which turns every downstream range into a hang the test timeout
exposes. The filter stage shows that changing the element count (dropping
non-matching events) does not change the contract at all. When you need a third
stage, it has the same shape and snaps on the same way; that uniformity is the whole
payoff of pipelines.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical treatment of stages, close-propagation, and fan-out.
- [Go Language Spec: Channel types](https://go.dev/ref/spec#Channel_types) — directional channel types `<-chan T` and `chan<- T`.
- [`strings.Cut`](https://pkg.go.dev/strings#Cut) — the split used by the parse stage.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-synchronous-handoff-backpressure.md](04-synchronous-handoff-backpressure.md) | Next: [06-safe-publish-after-close.md](06-safe-publish-after-close.md)
