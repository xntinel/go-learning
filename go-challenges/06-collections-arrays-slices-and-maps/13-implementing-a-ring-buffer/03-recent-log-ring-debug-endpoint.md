# Exercise 3: Bounded Recent-Log Ring Behind a /debug/logs Handler

A frequent operator request is "show me the last N log lines from this process,
right now, over HTTP." You cannot keep every line in memory, and you must not hand
the JSON encoder a live view of the buffer while a writer mutates it. This module
builds a `RecentLogs` component around a ring and an `http.Handler` that serves a
`Snapshot` as JSON — the freshest N entries, older ones dropped, and no aliasing of
live storage.

Self-contained: its own module, an inlined ring, the `RecentLogs` wrapper, the
handler, a demo, and `httptest`-based tests.

## What you'll build

```text
recentlogs/                independent module: example.com/recentlogs
  go.mod                   go 1.24
  recentlogs.go            LogEntry, Ring, RecentLogs (Append, Handler)
  cmd/
    demo/
      main.go              append past capacity, print the JSON the handler serves
  recentlogs_test.go       httptest: last-N in order, older dropped, copy isolation
```

Files: `recentlogs.go`, `cmd/demo/main.go`, `recentlogs_test.go`.
Implement: `LogEntry`, a mutex-guarded `RecentLogs` with `Append(LogEntry)`, and `ServeHTTP` returning a JSON array of the last N.
Test: append more than capacity, GET, decode, assert exactly the last N in order; mutate the ring after snapshot and confirm an already-encoded body is unaffected.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the handler must serve a Snapshot, not the buffer

The whole design pressure of this module is one sentence: the marshaller must
never touch the live ring. If `ServeHTTP` ranged the internal array directly while
a concurrent `Append` overwrote the oldest slot, `encoding/json` would read a torn
value — a half-written struct, or a slice header whose length no longer matches its
backing array. Worse, handing the encoder a sub-slice of the internal storage would
pin that array for as long as the response takes to write, which under a slow
client is unbounded. So `ServeHTTP` calls `Snapshot` under the lock to get an
independent copy, releases the lock, and only then encodes. The encoder now walks a
private slice that no writer can reach.

`RecentLogs` guards `Append` and `Snapshot` with a `sync.Mutex` because the handler
runs on many goroutines (one per request) while the application appends from its
own. The ring itself is not concurrency-safe — the mutex in `RecentLogs` is what
makes the pair safe.

### The shape of a LogEntry

A recent-log entry is a small structured record: a timestamp, a level, and a
message. Keeping it a plain struct with JSON tags means the handler is a
three-line `Encode` call and the operator gets clean JSON. The ring stores
`LogEntry` values, so eviction of the oldest is automatic once capacity is reached.

Create `recentlogs.go`:

```go
package recentlogs

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// LogEntry is one structured recent-log line.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// ring is a fixed-capacity FIFO buffer; when full, Push overwrites the oldest.
type ring[T any] struct {
	data []T
	head int
	tail int
	size int
}

func newRing[T any](capacity int) *ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &ring[T]{data: make([]T, capacity)}
}

func (r *ring[T]) push(v T) {
	r.data[r.head] = v
	r.head = (r.head + 1) % len(r.data)
	if r.size < len(r.data) {
		r.size++
	} else {
		r.tail = (r.tail + 1) % len(r.data)
	}
}

func (r *ring[T]) snapshot() []T {
	out := make([]T, r.size)
	for i := range r.size {
		out[i] = r.data[(r.tail+i)%len(r.data)]
	}
	return out
}

// RecentLogs keeps the most recent N log entries in memory and serves them as
// JSON. It is safe for concurrent Append and ServeHTTP.
type RecentLogs struct {
	mu sync.Mutex
	r  *ring[LogEntry]
}

// NewRecentLogs returns a buffer holding at most capacity entries.
func NewRecentLogs(capacity int) *RecentLogs {
	return &RecentLogs{r: newRing[LogEntry](capacity)}
}

// Append records one entry, overwriting the oldest when full.
func (rl *RecentLogs) Append(e LogEntry) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.r.push(e)
}

// Snapshot returns an independent copy of the current entries, oldest first.
func (rl *RecentLogs) Snapshot() []LogEntry {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.r.snapshot()
}

// ServeHTTP writes the recent entries as a JSON array. It encodes a Snapshot, so
// the encoder never races or pins the live buffer.
func (rl *RecentLogs) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	entries := rl.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

### The runnable demo

The demo appends four entries into a capacity-3 buffer (so the first is dropped),
then encodes the snapshot to stdout with fixed timestamps so the output is
deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"time"

	"example.com/recentlogs"
)

func main() {
	rl := recentlogs.NewRecentLogs(3)
	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	levels := []string{"info", "info", "warn", "error"}
	for i, lvl := range levels {
		rl.Append(recentlogs.LogEntry{
			Time:    base.Add(time.Duration(i) * time.Second),
			Level:   lvl,
			Message: "event",
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rl.Snapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
[
  {
    "time": "2026-07-02T10:00:01Z",
    "level": "info",
    "message": "event"
  },
  {
    "time": "2026-07-02T10:00:02Z",
    "level": "warn",
    "message": "event"
  },
  {
    "time": "2026-07-02T10:00:03Z",
    "level": "error",
    "message": "event"
  }
]
```

The `10:00:00` entry was the fourth-from-newest and dropped; the buffer serves the
last three in order.

### Tests

The tests drive the handler through `httptest` and decode the JSON body. The first
asserts that appending more than capacity yields exactly the last N in order. The
second is the isolation proof: capture the handler's body, then append more to the
ring, and confirm the already-written body is unchanged — because it was encoded
from a copy taken at request time, later writes cannot reach it.

Create `recentlogs_test.go`:

```go
package recentlogs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mkEntry(i int) LogEntry {
	return LogEntry{
		Time:    time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
		Level:   "info",
		Message: "m",
	}
}

func TestHandlerServesLastN(t *testing.T) {
	t.Parallel()
	rl := NewRecentLogs(3)
	for i := 1; i <= 5; i++ {
		rl.Append(mkEntry(i))
	}

	req := httptest.NewRequest(http.MethodGet, "/debug/logs", nil)
	rec := httptest.NewRecorder()
	rl.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var got []LogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(got))
	}
	for i, sec := range []int{3, 4, 5} {
		if got[i].Time.Second() != sec {
			t.Fatalf("entry %d second = %d, want %d (wrong order or wrong dropped set)",
				i, got[i].Time.Second(), sec)
		}
	}
}

func TestResponseBodyIsIndependentOfLiveBuffer(t *testing.T) {
	t.Parallel()
	rl := NewRecentLogs(3)
	rl.Append(mkEntry(1))
	rl.Append(mkEntry(2))
	rl.Append(mkEntry(3))

	rec := httptest.NewRecorder()
	rl.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/logs", nil))
	body := append([]byte(nil), rec.Body.Bytes()...) // capture the served bytes

	// Mutate the live buffer after the response was written.
	rl.Append(mkEntry(4))
	rl.Append(mkEntry(5))

	var got []LogEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}
	if len(got) != 3 || got[0].Time.Second() != 1 || got[2].Time.Second() != 3 {
		t.Fatalf("captured body changed after later Appends: %v", got)
	}
}

func TestConcurrentAppendAndServe(t *testing.T) {
	t.Parallel()
	rl := NewRecentLogs(16)
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			rl.Append(mkEntry(i))
		}
		close(done)
	}()
	for {
		rec := httptest.NewRecorder()
		rl.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/logs", nil))
		select {
		case <-done:
			return
		default:
		}
	}
}
```

## Review

The endpoint is correct when it serves exactly the freshest N entries in order and
its response is a snapshot the writer cannot reach. `TestHandlerServesLastN` pins
the drop-oldest behavior through real HTTP machinery; `TestResponseBodyIsIndependentOfLiveBuffer`
is the one that fails if you ever encode the internal array directly instead of a
copy — it appends to the ring after the response and checks the captured bytes did
not change. `TestConcurrentAppendAndServe` under `-race` proves the mutex actually
guards `Append` against `ServeHTTP`. The trap to avoid is optimizing away the
snapshot "to save an allocation": the allocation is the isolation, and losing it
trades a microsecond for a race and a heap pin under a slow client.

## Resources

- [`net/http` package](https://pkg.go.dev/net/http) — `http.Handler`, `ServeHTTP`, `http.Error`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest`, `NewRecorder` for handler tests.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `json.NewEncoder`, `SetIndent`, `Unmarshal`.

---

Back to [02-eviction-and-snapshot-contract-tests.md](02-eviction-and-snapshot-contract-tests.md) | Next: [04-thread-safe-ring-mutex.md](04-thread-safe-ring-mutex.md)
