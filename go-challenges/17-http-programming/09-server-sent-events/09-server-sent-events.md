# 9. Server-Sent Events

Server-Sent Events stream server-to-client messages over ordinary HTTP. This lesson builds a reusable `sse` package with event formatting, a broker for multiple subscribers, and an HTTP handler that flushes each event.

## Concepts

An SSE response uses `Content-Type: text/event-stream`. Each message is text fields such as `id:`, `event:`, `retry:`, and one or more `data:` lines, followed by a blank line. In Go, handlers can flush buffered data with the documented `http.Flusher` interface. Client disconnects are observed through `r.Context().Done()`.

A broker keeps one buffered channel per subscriber. Publishing should not let one slow subscriber block all others, so this package drops messages when a subscriber channel is full.

## Exercises

Create this module layout:

```text
sse-streams/
    go.mod
    sse.go
    sse_example_test.go
    sse_test.go
    cmd/demo/main.go
```

Create `go.mod`:

```go
module example.com/sse-streams

go 1.26
```

Create `sse.go`:

```go
package sse

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

var ErrInvalidEvent = errors.New("invalid event")

type Event struct {
	ID    string
	Name  string
	Data  string
	Retry int
}

func (e Event) Format() (string, error) {
	if e.Data == "" && e.Retry == 0 {
		return "", fmt.Errorf("%w: empty data and retry", ErrInvalidEvent)
	}
	var b strings.Builder
	if e.Retry > 0 {
		fmt.Fprintf(&b, "retry: %d\n", e.Retry)
	}
	if e.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", e.ID)
	}
	if e.Name != "" {
		fmt.Fprintf(&b, "event: %s\n", e.Name)
	}
	if e.Data != "" {
		for _, line := range strings.Split(e.Data, "\n") {
			fmt.Fprintf(&b, "data: %s\n", line)
		}
	}
	b.WriteByte('\n')
	return b.String(), nil
}

type Broker struct {
	mu          sync.RWMutex
	subscribers map[chan Event]struct{}
	buffer      int
}

func NewBroker(buffer int) *Broker {
	if buffer < 1 {
		buffer = 1
	}
	return &Broker{subscribers: make(map[chan Event]struct{}), buffer: buffer}
}

func (b *Broker) Subscribe() chan Event {
	ch := make(chan Event, b.buffer)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *Broker) Publish(event Event) int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	sent := 0
	for ch := range b.subscribers {
		select {
		case ch <- event:
			sent++
		default:
		}
	}
	return sent
}

func (b *Broker) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

func (b *Broker) EventsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		initial, err := (Event{Retry: 3000}).Format()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, initial)
		flusher.Flush()

		ch := b.Subscribe()
		defer b.Unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				formatted, err := event.Format()
				if err != nil {
					continue
				}
				fmt.Fprint(w, formatted)
				flusher.Flush()
			}
		}
	}
}
```

Create `sse_example_test.go`:

```go
package sse_test

import (
	"fmt"

	"example.com/sse-streams"
)

func ExampleEvent_Format() {
	formatted, err := sse.Event{ID: "1", Name: "message", Data: "hello"}.Format()
	fmt.Print(formatted)
	fmt.Println(err == nil)

	// Output:
	// id: 1
	// event: message
	// data: hello
	//
	// true
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"example.com/sse-streams"
)

func main() {
	broker := sse.NewBroker(10)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", broker.EventsHandler())
	mux.HandleFunc("POST /publish", func(w http.ResponseWriter, r *http.Request) {
		msg := r.URL.Query().Get("msg")
		if msg == "" {
			http.Error(w, "msg query parameter required", http.StatusBadRequest)
			return
		}
		count := broker.Publish(sse.Event{ID: strconv.FormatInt(time.Now().UnixNano(), 10), Name: "message", Data: msg})
		fmt.Fprintf(w, "published to %d subscribers\n", count)
	})

	log.Fatal(http.ListenAndServe(":8080", mux))
}
```

Create `sse_test.go`:

```go
package sse

import (
	"bufio"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventFormatValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event Event
	}{
		{name: "empty", event: Event{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.event.Format()
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("expected ErrInvalidEvent, got %v", err)
			}
		})
	}
}

func TestBrokerPublishAndCount(t *testing.T) {
	t.Parallel()

	broker := NewBroker(1)
	first := broker.Subscribe()
	second := broker.Subscribe()
	if broker.Count() != 2 {
		t.Fatalf("count = %d", broker.Count())
	}

	sent := broker.Publish(Event{Data: "hello"})
	if sent != 2 {
		t.Fatalf("sent = %d", sent)
	}

	for name, ch := range map[string]chan Event{"first": first, "second": second} {
		select {
		case event := <-ch:
			if event.Data != "hello" {
				t.Fatalf("%s received %q", name, event.Data)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive event", name)
		}
	}

	broker.Unsubscribe(first)
	broker.Unsubscribe(second)
	if broker.Count() != 0 {
		t.Fatalf("count after unsubscribe = %d", broker.Count())
	}
}

func TestEventsHandlerSendsRetryAndDisconnects(t *testing.T) {
	t.Parallel()

	broker := NewBroker(1)
	server := httptest.NewServer(broker.EventsHandler())
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	text := readUntil(t, reader, "retry: 3000")
	if !strings.Contains(text, "retry: 3000") {
		t.Fatalf("missing retry directive: %q", text)
	}

	waitForCount(t, broker, 1)
	broker.Publish(Event{ID: "1", Name: "message", Data: "hello"})
	text += readUntil(t, reader, "data: hello")
	if !strings.Contains(text, "event: message") {
		t.Fatalf("missing event body: %q", text)
	}

	resp.Body.Close()
	waitForCount(t, broker, 0)
}

func readUntil(t *testing.T, reader *bufio.Reader, want string) string {
	t.Helper()

	type result struct {
		line string
		err  error
	}
	var b strings.Builder
	deadline := time.After(time.Second)
	for {
		ch := make(chan result, 1)
		go func() {
			line, err := reader.ReadString('\n')
			ch <- result{line: line, err: err}
		}()

		select {
		case res := <-ch:
			if res.err != nil {
				t.Fatalf("ReadString: %v", res.err)
			}
			b.WriteString(res.line)
			if strings.Contains(b.String(), want) {
				return b.String()
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q in %q", want, b.String())
		}
	}
}

func waitForCount(t *testing.T, broker *Broker, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if broker.Count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("count = %d, want %d", broker.Count(), want)
}
```

## Common Mistakes

- Forgetting the blank line that terminates each SSE message.
- Omitting `http.Flusher` and assuming buffered writes reach the client immediately.
- Letting one slow subscriber block all publishers and subscribers.
- Ignoring `r.Context().Done()` and leaking subscribers after clients disconnect.
- Sending invalid events to the stream instead of validating and wrapping errors.

## Verification

Run these commands from the module root:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

You built an SSE formatter, a concurrent broker, a streaming HTTP handler with `http.Flusher`, disconnection cleanup through request context cancellation, and tests that verify subscribers, formatting, and handler behavior with `httptest`.

## What's Next

Next: [WebSocket Server](../10-websocket-server/10-websocket-server.md).

## Resources

- [net/http Flusher](https://pkg.go.dev/net/http#Flusher)
- [net/http Request.Context](https://pkg.go.dev/net/http#Request.Context)
- [MDN Server-sent events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
- [HTML Living Standard Server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
