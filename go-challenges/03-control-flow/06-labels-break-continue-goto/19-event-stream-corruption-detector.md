# Exercise 19: Stop all event streams the moment corruption is detected

**Nivel: Intermedio** — validacion rapida (un test corto).

A backend consumes events from several independent streams at once — a
Kafka topic, an SQS queue, a PubSub subscription — and feeds them into a
shared aggregation step. A corruption sentinel event in any one stream means
the aggregation state built from everything seen so far cannot be trusted,
so nothing after it — in that stream, or in any other stream still waiting
to be scanned — should be processed. This module is fully self-contained:
its own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
corruption/                 independent module: example.com/corruption
  go.mod                     go 1.24
  corruption.go              Event, Stream, ScanStreams
  cmd/
    demo/
      main.go                runnable demo: three streams, corruption in the second
  corruption_test.go          table test: no streams, all clean, halt in stream one, halt in a later stream, corrupt-first-event
```

- Files: `corruption.go`, `cmd/demo/main.go`, `corruption_test.go`.
- Implement: `ScanStreams(streams []Stream) (processed int, corruptStream string, corruptIndex int, corrupted bool)`, halting every stream the instant a corrupt event is found in any of them.
- Test: no streams at all, every stream clean, corruption in the first stream, corruption in a later stream (with earlier streams already counted), and a corrupt event as the very first event of a stream.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/06-labels-break-continue-goto/19-event-stream-corruption-detector/cmd/demo
cd go-solutions/03-control-flow/06-labels-break-continue-goto/19-event-stream-corruption-detector
go mod edit -go=1.24
```

### Why one corrupt event stops every stream, not just its own

`ScanStreams` walks streams in an outer loop and each stream's events in an
inner loop. A bare `break` on finding a corrupt event would only leave the
inner loop — the outer loop would then move on to the *next* stream and keep
consuming it, which is precisely wrong here: the whole point of a corruption
sentinel is that shared downstream state is now suspect, so continuing to
feed it more events from any source only compounds the damage. The labeled
`break streams`, fired from inside the per-event loop, leaves both loops in
one statement: the rest of the corrupted stream and every stream still
waiting are all left untouched. `processed` only ever counts events that
were confirmed clean before the halt, so it doubles as an audit trail of
exactly how much of the combined input was safe to trust.

Create `corruption.go`:

```go
package corruption

// Event is one message consumed off a stream.
type Event struct {
	ID      string
	Corrupt bool
}

// Stream is one independent source: a Kafka topic, an SQS queue, a PubSub
// subscription.
type Stream struct {
	Name   string
	Events []Event
}

// ScanStreams processes every stream's events in order. A corrupt event is
// a sentinel that data integrity for the ENTIRE consumer is compromised —
// not just this stream — because streams typically share a downstream
// aggregation step that cannot be trusted once any input is suspect. The
// instant a corrupt event is found, EVERY remaining event in EVERY stream
// (including the rest of the current one) must be left unprocessed. A
// labeled break on the streams loop, fired from inside the per-event loop,
// stops both loops in one statement.
func ScanStreams(streams []Stream) (processed int, corruptStream string, corruptIndex int, corrupted bool) {
streams:
	for _, s := range streams {
		for i, e := range s.Events {
			if e.Corrupt {
				corruptStream = s.Name
				corruptIndex = i
				corrupted = true
				break streams
			}
			processed++
		}
	}
	return processed, corruptStream, corruptIndex, corrupted
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/corruption"
)

func main() {
	streams := []corruption.Stream{
		{
			Name: "kafka.orders",
			Events: []corruption.Event{
				{ID: "o1"}, {ID: "o2"}, {ID: "o3"},
			},
		},
		{
			Name: "sqs.payments",
			Events: []corruption.Event{
				{ID: "p1"}, {ID: "p2", Corrupt: true}, {ID: "p3"},
			},
		},
		{
			Name: "pubsub.shipping",
			Events: []corruption.Event{
				{ID: "s1"},
			},
		},
	}

	processed, stream, idx, corrupted := corruption.ScanStreams(streams)
	fmt.Println("processed:", processed)
	fmt.Println("corrupted:", corrupted)
	if corrupted {
		fmt.Printf("halted at %s[%d]\n", stream, idx)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: 4
corrupted: true
halted at sqs.payments[1]
```

All three events of `kafka.orders` are clean and counted, plus the first
event of `sqs.payments` — four in total — before its second event trips the
corruption sentinel. `p3` and the entire `pubsub.shipping` stream are never
touched.

### Tests

`TestScanStreams` covers no streams at all, every stream clean, corruption
in the very first stream, corruption in a later stream after earlier ones
were already counted clean, and a corrupt event sitting at index zero of its
stream.

Create `corruption_test.go`:

```go
package corruption

import "testing"

func TestScanStreams(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		streams           []Stream
		wantProcessed     int
		wantCorrupted     bool
		wantCorruptStream string
		wantCorruptIndex  int
	}{
		"no streams": {
			streams:       nil,
			wantProcessed: 0,
		},
		"all clean across multiple streams": {
			streams: []Stream{
				{Name: "a", Events: []Event{{ID: "1"}, {ID: "2"}}},
				{Name: "b", Events: []Event{{ID: "3"}}},
			},
			wantProcessed: 3,
		},
		"corruption in the first stream halts before the second is touched": {
			streams: []Stream{
				{Name: "a", Events: []Event{{ID: "1"}, {ID: "2", Corrupt: true}, {ID: "3"}}},
				{Name: "b", Events: []Event{{ID: "4"}}},
			},
			wantProcessed:     1,
			wantCorrupted:     true,
			wantCorruptStream: "a",
			wantCorruptIndex:  1,
		},
		"corruption in a later stream still reports events processed before it": {
			streams: []Stream{
				{Name: "a", Events: []Event{{ID: "1"}, {ID: "2"}}},
				{Name: "b", Events: []Event{{ID: "3"}, {ID: "4", Corrupt: true}}},
				{Name: "c", Events: []Event{{ID: "5"}}},
			},
			wantProcessed:     3,
			wantCorrupted:     true,
			wantCorruptStream: "b",
			wantCorruptIndex:  1,
		},
		"a corrupt event as the very first event of a stream": {
			streams: []Stream{
				{Name: "a", Events: []Event{{ID: "1", Corrupt: true}}},
			},
			wantProcessed:     0,
			wantCorrupted:     true,
			wantCorruptStream: "a",
			wantCorruptIndex:  0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			processed, stream, idx, corrupted := ScanStreams(tc.streams)
			if processed != tc.wantProcessed || corrupted != tc.wantCorrupted ||
				stream != tc.wantCorruptStream || idx != tc.wantCorruptIndex {
				t.Fatalf("ScanStreams = (%d, %q, %d, %v), want (%d, %q, %d, %v)",
					processed, stream, idx, corrupted,
					tc.wantProcessed, tc.wantCorruptStream, tc.wantCorruptIndex, tc.wantCorrupted)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The detector is correct when `processed` counts only events strictly before
the corruption point, and every event in every stream after it — including
the rest of the corrupted stream itself — is left untouched. The "corruption
in a later stream" test is the one to study: it proves earlier streams were
genuinely scanned and counted, not skipped, while confirming the halt still
reaches back out through both loops from deep inside the event scan. The bug
this exercise guards against is a bare `break` on detecting corruption: it
would only stop the current stream, and the outer loop would happily start
consuming the next one, feeding more data into a state that is already
known to be compromised.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [Apache Kafka: consumer fundamentals](https://kafka.apache.org/documentation/#consumerapi) — the shape of a real multi-topic consumer this scanner models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-token-bucket-rate-limiter.md](18-token-bucket-rate-limiter.md) | Next: [20-failure-rate-circuit-breaker.md](20-failure-rate-circuit-breaker.md)
