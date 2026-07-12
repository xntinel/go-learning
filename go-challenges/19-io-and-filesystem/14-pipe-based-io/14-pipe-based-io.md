# 14. Pipe-Based I/O

Build a streaming JSON encoder around `io.Pipe`. The lesson focuses on the contract between a producer goroutine and a consumer: the writer must close the pipe, errors must travel through `CloseWithError`, and tests must avoid goroutine leaks.

## Concepts

### io.Pipe Connects A Writer To A Reader

`io.Pipe` creates a synchronous in-memory pipe. Writes block until reads consume data. That backpressure is useful for streaming, but it also means every producer needs a consumer.

### CloseWithError Preserves Producer Failures

If a goroutine fails while producing data, closing the writer with `CloseWithError` lets the reader observe that failure. Closing with nil makes the stream look successful.

### Pipes Need Ownership

The producer owns the writer and closes it exactly once. The consumer owns the reader. Mixing ownership is the usual source of deadlocks and hidden errors.

## Exercises

### Exercise 1: Implement A Streaming Encoder

Create `pipe.go`:

```go
package jsonpipe

import (
	"encoding/json"
	"fmt"
	"io"
)

type Event struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func Stream(events []Event) (io.ReadCloser, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("stream events: %w", ErrNoEvents)
	}
	reader, writer := io.Pipe()
	go func() {
		enc := json.NewEncoder(writer)
		for _, event := range events {
			if event.ID < 1 {
				_ = writer.CloseWithError(fmt.Errorf("event %q: %w", event.Name, ErrInvalidID))
				return
			}
			if err := enc.Encode(event); err != nil {
				_ = writer.CloseWithError(fmt.Errorf("encode event %d: %w", event.ID, err))
				return
			}
		}
		_ = writer.Close()
	}()
	return reader, nil
}
```

Create `errors.go`:

```go
package jsonpipe

import "errors"

var (
	ErrNoEvents  = errors.New("at least one event is required")
	ErrInvalidID = errors.New("event id must be positive")
)
```

### Exercise 2: Test Success And Producer Errors

Create `pipe_test.go`:

```go
package jsonpipe

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestStreamEncodesEvents(t *testing.T) {
	t.Parallel()

	r, err := Stream([]Event{{ID: 1, Name: "start"}, {ID: 2, Name: "stop"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"start"`) || !strings.Contains(string(data), `"name":"stop"`) {
		t.Fatalf("data = %s", data)
	}
}

func TestStreamRejectsEmptyInput(t *testing.T) {
	t.Parallel()

	_, err := Stream(nil)
	if !errors.Is(err, ErrNoEvents) {
		t.Fatalf("err = %v, want ErrNoEvents", err)
	}
}

func TestStreamReturnsProducerErrorToReader(t *testing.T) {
	t.Parallel()

	r, err := Stream([]Event{{ID: 0, Name: "bad"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = io.ReadAll(r)
	if !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
}

func ExampleStream() {
	r, _ := Stream([]Event{{ID: 1, Name: "demo"}})
	defer r.Close()
	data, _ := io.ReadAll(r)
	fmt.Print(string(data))
	// Output:
	// {"id":1,"name":"demo"}
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"io"
	"log"
	"os"

	"example.com/jsonpipe"
)

func main() {
	r, err := jsonpipe.Stream([]jsonpipe.Event{{ID: 1, Name: "demo"}})
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	if _, err := io.Copy(os.Stdout, r); err != nil {
		log.Fatal(err)
	}
}
```

## Common Mistakes

### No Consumer For The Pipe

Wrong: write to an `io.PipeWriter` in the same goroutine before starting a reader.

Fix: run the producer in a goroutine and return the reader immediately.

### Losing Producer Errors

Wrong: call `writer.Close()` after validation fails.

Fix: call `writer.CloseWithError(fmt.Errorf("...: %w", ErrInvalidID))`.

### Forgetting To Close The Reader

Wrong: leave the returned reader open in tests.

Fix: `defer r.Close()` after successful construction.

## Verification

Run this from `~/go-exercises/jsonpipe`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test with three events and count newline-delimited JSON records.

## Summary

- `io.Pipe` connects a producer writer to a consumer reader with backpressure.
- Producer failures should use `CloseWithError`.
- Pipe ownership must be clear to avoid deadlocks.
- Tests should consume and close returned pipe readers.

## What's Next

Next: [Memory-Mapped Files](../15-memory-mapped-files/15-memory-mapped-files.md).

## Resources

- [io.Pipe](https://pkg.go.dev/io#Pipe)
- [encoding/json Encoder](https://pkg.go.dev/encoding/json#Encoder)
- [io.ReadAll](https://pkg.go.dev/io#ReadAll)
