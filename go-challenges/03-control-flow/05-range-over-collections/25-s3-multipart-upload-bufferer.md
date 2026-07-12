# Exercise 25: Track and Buffer Multipart Upload Parts with Completion Detection

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An S3-style multipart upload lets a client send an object's bytes as
independent, possibly out-of-order, possibly retried parts, and the server
must track which parts have arrived for each in-flight upload, detect when
every expected part is present, and reclaim the memory of uploads a client
abandoned mid-transfer without ever exceeding a bounded buffer budget shared
across every upload in flight. This module ranges each upload's parts to
detect completion, ranges all in-flight uploads under lock to evict
abandoned ones on timeout, and enforces the shared byte budget on every
accepted part — a state machine under concurrency and memory pressure. The
module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
multipart/                  independent module: example.com/s3-multipart-upload-bufferer
  go.mod                    go 1.24
  multipart.go              type Manager; AddPart, Complete, SweepAbandoned
  cmd/
    demo/
      main.go               runnable demo: assemble a 3-part upload, sweep an abandoned one
  multipart_test.go         table test: completion + buffer-full + abandoned sweep; concurrent AddPart under -race
```

- Files: `multipart.go`, `cmd/demo/main.go`, `multipart_test.go`.
- Implement: `Manager.AddPart`, `Manager.Complete`, and
  `Manager.SweepAbandoned`, all synchronized under one `sync.Mutex`.
- Test: one table for `Complete` (unknown, incomplete, complete-out-of-
  order), a dedicated buffer-full and idempotent-resend test, a
  `SweepAbandoned` test, and a concurrent `AddPart` test under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Detecting completion by ranging expected part numbers, not by counting

`isComplete` does not just check `len(u.parts) == u.totalParts` and stop
there — a length match alone cannot tell you the *right* parts arrived
versus, say, part 1 arriving twice under two different keys somehow, or a
gap where part 2 never came and part 4 arrived instead of a legitimate
part 3. The length check is a fast rejection for the common "still waiting"
case, but the actual proof of completeness is the loop that ranges
`1..totalParts` and confirms every expected part number has an entry — this
is a range over the *expected* index space, not over `u.parts` itself, which
is the detail that catches a hypothetical missing-middle-part bug that a
naive length check would miss.

Every mutating method — `AddPart`, `Complete`, `SweepAbandoned` — holds
`m.mu` for its entire body, because the shared `usedBytes` counter and the
per-upload `parts` maps are both touched by any of the three concurrently
from different goroutines (one per client connection uploading a part,
one background sweeper). `AddPart` also treats a re-sent part number as an
idempotent no-op rather than double-charging the buffer budget for it — an
S3 client is expected to retry a part upload on a transient failure, and
that retry must not silently shrink the room left for other uploads by
counting the same bytes twice. `SweepAbandoned` ranges `m.uploads` under
that same lock to find every upload whose `lastActive` has aged past
`timeout`, deletes it, and returns its bytes to `usedBytes` — reclaiming
memory a client's dropped connection would otherwise hold onto forever.

Create `multipart.go`:

```go
package multipart

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrBufferFull is returned by AddPart when accepting the part would exceed
// the manager's total buffer budget.
var ErrBufferFull = errors.New("multipart: buffer full")

// upload tracks the in-flight parts for one multipart upload.
type upload struct {
	totalParts int
	parts      map[int][]byte
	bytesUsed  int
	lastActive time.Time
}

func (u *upload) isComplete() bool {
	if len(u.parts) != u.totalParts {
		return false
	}
	for i := 1; i <= u.totalParts; i++ {
		if _, ok := u.parts[i]; !ok {
			return false
		}
	}
	return true
}

// Manager tracks in-flight multipart uploads under a bounded shared buffer.
// All methods are safe for concurrent use.
type Manager struct {
	mu        sync.Mutex
	uploads   map[string]*upload
	maxBytes  int
	usedBytes int
}

// NewManager builds a Manager that will not buffer more than maxBytes across
// all in-flight uploads combined.
func NewManager(maxBytes int) *Manager {
	return &Manager{
		uploads:  make(map[string]*upload),
		maxBytes: maxBytes,
	}
}

// AddPart buffers one part of an upload. A part re-sent with the same part
// number is treated as an idempotent duplicate and ignored, not double-
// counted against the buffer budget. It returns ErrBufferFull if accepting
// the part would exceed the manager's total byte budget.
func (m *Manager) AddPart(uploadID string, totalParts, partNum int, data []byte, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.uploads[uploadID]
	if !ok {
		u = &upload{totalParts: totalParts, parts: make(map[int][]byte)}
		m.uploads[uploadID] = u
	}
	if _, dup := u.parts[partNum]; dup {
		u.lastActive = now
		return nil // idempotent re-send of an already-buffered part
	}
	if m.usedBytes+len(data) > m.maxBytes {
		return ErrBufferFull
	}

	u.parts[partNum] = data
	u.bytesUsed += len(data)
	u.lastActive = now
	m.usedBytes += len(data)
	return nil
}

// Complete assembles the final object if every expected part has arrived,
// removes the upload, and frees its buffered bytes. The second return is
// false if the upload is unknown or still incomplete.
func (m *Manager) Complete(uploadID string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.uploads[uploadID]
	if !ok || !u.isComplete() {
		return nil, false
	}

	var buf []byte
	for i := 1; i <= u.totalParts; i++ {
		buf = append(buf, u.parts[i]...)
	}

	delete(m.uploads, uploadID)
	m.usedBytes -= u.bytesUsed
	return buf, true
}

// SweepAbandoned ranges all in-flight uploads under lock and evicts every one
// whose last activity is older than timeout relative to now, freeing their
// buffered bytes back to the shared budget. It returns the evicted upload
// IDs, sorted for a deterministic result.
func (m *Manager) SweepAbandoned(now time.Time, timeout time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var evicted []string
	for id, u := range m.uploads {
		if now.Sub(u.lastActive) >= timeout {
			delete(m.uploads, id)
			m.usedBytes -= u.bytesUsed
			evicted = append(evicted, id)
		}
	}
	sort.Strings(evicted)
	return evicted
}

// UsedBytes reports how many bytes are currently buffered across all
// in-flight uploads.
func (m *Manager) UsedBytes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usedBytes
}
```

### The runnable demo

The demo drives one upload to completion (its parts arriving in order,
proving `Complete` refuses to assemble early), then leaves a second upload
abandoned and sweeps it after its timeout.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/s3-multipart-upload-bufferer"
)

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := multipart.NewManager(1024)

	m.AddPart("upload-1", 3, 1, []byte("aaa"), base)
	m.AddPart("upload-1", 3, 2, []byte("bbb"), base.Add(1*time.Second))

	if _, ok := m.Complete("upload-1"); ok {
		fmt.Println("unexpected: complete before all parts arrived")
	}

	m.AddPart("upload-1", 3, 3, []byte("ccc"), base.Add(2*time.Second))
	data, ok := m.Complete("upload-1")
	fmt.Printf("complete=%v data=%q used=%d\n", ok, data, m.UsedBytes())

	m.AddPart("upload-2", 2, 1, []byte("xyz"), base)
	evicted := m.SweepAbandoned(base.Add(10*time.Minute), 5*time.Minute)
	fmt.Printf("evicted=%v used=%d\n", evicted, m.UsedBytes())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
complete=true data="aaabbbccc" used=0
evicted=[upload-2] used=0
```

### Tests

The `Complete` table covers an unknown upload, a genuinely incomplete one,
and a complete one whose parts arrived out of number order. A dedicated test
proves `ErrBufferFull` fires exactly at the budget boundary and that
resending an already-buffered part is a free no-op. Another test proves
`SweepAbandoned` evicts only uploads past their timeout. A concurrency test
runs many goroutines calling `AddPart` at once under `-race`.

Create `multipart_test.go`:

```go
package multipart

import (
	"sync"
	"testing"
	"time"
)

func TestAddPartAndComplete(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		build    func(m *Manager)
		uploadID string
		wantData string
		wantOK   bool
	}{
		{
			name:     "unknown upload",
			build:    func(m *Manager) {},
			uploadID: "ghost",
			wantData: "",
			wantOK:   false,
		},
		{
			name: "incomplete upload is not ready",
			build: func(m *Manager) {
				m.AddPart("u1", 2, 1, []byte("a"), base)
			},
			uploadID: "u1",
			wantData: "",
			wantOK:   false,
		},
		{
			name: "all parts present assembles in part-number order",
			build: func(m *Manager) {
				m.AddPart("u1", 3, 3, []byte("ccc"), base)
				m.AddPart("u1", 3, 1, []byte("aaa"), base)
				m.AddPart("u1", 3, 2, []byte("bbb"), base)
			},
			uploadID: "u1",
			wantData: "aaabbbccc",
			wantOK:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(1024)
			tc.build(m)

			data, ok := m.Complete(tc.uploadID)
			if ok != tc.wantOK {
				t.Fatalf("Complete() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && string(data) != tc.wantData {
				t.Fatalf("Complete() data = %q, want %q", data, tc.wantData)
			}
		})
	}
}

func TestAddPartBufferFull(t *testing.T) {
	t.Parallel()

	m := NewManager(5)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if err := m.AddPart("u1", 2, 1, []byte("abc"), base); err != nil {
		t.Fatalf("AddPart() err = %v, want nil", err)
	}
	if err := m.AddPart("u1", 2, 2, []byte("defgh"), base); err != ErrBufferFull {
		t.Fatalf("AddPart() err = %v, want ErrBufferFull", err)
	}
	// Re-sending the already-buffered part 1 must be an idempotent no-op,
	// not double-counted against the budget.
	if err := m.AddPart("u1", 2, 1, []byte("abc"), base); err != nil {
		t.Fatalf("AddPart() duplicate resend err = %v, want nil", err)
	}
	if m.UsedBytes() != 3 {
		t.Fatalf("UsedBytes() = %d, want 3", m.UsedBytes())
	}
}

func TestSweepAbandoned(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewManager(1024)

	m.AddPart("stale", 2, 1, []byte("aa"), base)
	m.AddPart("fresh", 2, 1, []byte("bb"), base.Add(9*time.Minute))

	evicted := m.SweepAbandoned(base.Add(10*time.Minute), 5*time.Minute)
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Fatalf("SweepAbandoned() = %v, want [stale]", evicted)
	}
	if m.UsedBytes() != 2 {
		t.Fatalf("UsedBytes() = %d, want 2", m.UsedBytes())
	}
}

func TestConcurrentAddPart(t *testing.T) {
	t.Parallel()

	m := NewManager(1 << 20)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "upload-" + string(rune('A'+i%5))
			m.AddPart(id, 4, i%4+1, []byte("data"), base)
		}(i)
	}
	wg.Wait()

	if m.UsedBytes() < 0 {
		t.Fatalf("UsedBytes() = %d, want >= 0", m.UsedBytes())
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The manager is correct when `Complete` only ever succeeds once every
expected part number is present, `AddPart` never lets total buffered bytes
exceed `maxBytes`, and a resent part never counts twice against that budget.
The bug this design specifically avoids is charging the buffer budget for a
retried part upload: without the `dup` check in `AddPart`, a client that
retries the same part three times because of a flaky connection would
consume three times the memory for identical bytes, eventually starving
other legitimate uploads of buffer space under exactly the kind of network
conditions that make retries necessary in the first place.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Go Specification: For statements (range over map, concurrency)](https://go.dev/ref/spec#For_range)
- [Amazon S3: Multipart upload overview](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html) — the real-world protocol this exercise's `Manager` models.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [24-write-ahead-log-replayer.md](24-write-ahead-log-replayer.md) | Next: [26-consistent-hashing-ring-sharding.md](26-consistent-hashing-ring-sharding.md)
