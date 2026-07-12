# Exercise 5: Make output deterministic — redact timestamps, UUIDs, durations

Real API responses carry `created_at`, a request ID, a latency. Snapshot those
bytes as-is and the test fails on every run. This module writes a normalizer that
replaces volatile fields with stable placeholders before comparison, so the golden
captures structure without flaking — the precondition that makes snapshotting
realistic payloads possible at all.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
redactsnap/                independent module: example.com/redactsnap
  go.mod                   go 1.26
  redact.go                Event, Render(Event) []byte, Normalize([]byte) []byte
  testdata/
    normalized.golden      normalized reference with placeholders
  cmd/
    demo/
      main.go              renders + normalizes a sample event
  redact_test.go           two different volatile payloads normalize to one golden
```

Files: `redact.go`, `testdata/normalized.golden`, `cmd/demo/main.go`, `redact_test.go`.
Implement: `Render(Event) []byte` (JSON with a trailing newline) and `Normalize([]byte) []byte` that redacts RFC3339 timestamps, UUIDs, and Go durations to `<TIMESTAMP>`, `<UUID>`, `<DURATION>`.
Test: feed two payloads with *different* timestamps, UUIDs, and latencies; assert both normalize to the same golden, proving no flake.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/05-redact-volatile-fields/cmd/demo go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/05-redact-volatile-fields/testdata
cd go-solutions/12-testing-ecosystem/23-snapshot-approval-testing/05-redact-volatile-fields
```

### Two ways to redact, and why regex here

There are two normalization strategies. *Redaction by field* unmarshals the output
into a `map[string]any`, overwrites the volatile keys, and re-serializes:

```go
// Illustrative: precise, but requires knowing the field names and re-serializing.
var m map[string]any
_ = json.Unmarshal(b, &m)
m["request_id"] = "<UUID>"
m["created_at"] = "<TIMESTAMP>"
out, _ := json.MarshalIndent(m, "", "  ")
```

It is precise — it only touches the keys you name, so it never rewrites a
legitimate value that merely looks like a UUID — but it needs you to know every
volatile key and to round-trip through a map. *Regex on bytes* runs patterns over
the serialized output and replaces every match. It is quick and works on output
whose shape you do not fully control, at the cost of fragility: a pattern too
greedy redacts real data, one too narrow misses a variant. This module uses the
regex approach because it demonstrates the ordering hazard directly and needs no
knowledge of the key names; prefer field-level redaction when you own the type.

Ordering of the substitutions is not cosmetic. The timestamp pattern runs first so
its digits are gone before the duration pattern (which matches `digits + unit`)
can see them; the UUID pattern runs before the duration pattern for the same
reason — a UUID's trailing hex block is all digits. Get the order wrong and the
duration regex chews a hole in a timestamp. The proof of correctness is
`TestNormalizeStableAcrossValues`: two payloads with entirely different volatile
values must normalize to the *same* golden. If they do, the volatility is gone and
the snapshot cannot flake.

`Render` uses `time.Time.Format(time.RFC3339)` for the timestamp and
`time.Duration.String()` for the latency, which are the exact serialized forms the
regexes target — the normalizer and the producer agree on the wire format.

Create `redact.go`:

```go
package redactsnap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Event is a request-scoped record whose id, timestamp, and latency are volatile.
type Event struct {
	RequestID string
	CreatedAt time.Time
	Latency   time.Duration
	User      string
}

// Render serializes e to indented JSON with one trailing newline, formatting the
// timestamp as RFC3339 and the latency with Duration.String — the exact forms the
// normalizer's patterns target.
func Render(e Event) []byte {
	wire := struct {
		RequestID string `json:"request_id"`
		CreatedAt string `json:"created_at"`
		Latency   string `json:"latency"`
		User      string `json:"user"`
	}{
		RequestID: e.RequestID,
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		Latency:   e.Latency.String(),
		User:      e.User,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("render: marshal: %v", err))
	}
	return append(data, '\n')
}

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	reUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reDuration  = regexp.MustCompile(`\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)`)
)

// Normalize redacts volatile substrings to stable placeholders. Order matters:
// timestamps and UUIDs are removed before the duration pattern, which matches
// digit-plus-unit, could otherwise carve into their digits.
func Normalize(b []byte) []byte {
	b = reTimestamp.ReplaceAll(b, []byte("<TIMESTAMP>"))
	b = reUUID.ReplaceAll(b, []byte("<UUID>"))
	b = reDuration.ReplaceAll(b, []byte("<DURATION>"))
	return b
}
```

Now the normalized reference:

Create `testdata/normalized.golden`:

```text
{
  "request_id": "<UUID>",
  "created_at": "<TIMESTAMP>",
  "latency": "<DURATION>",
  "user": "alice"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"
	"time"

	"example.com/redactsnap"
)

func main() {
	e := redactsnap.Event{
		RequestID: "550e8400-e29b-41d4-a716-446655440000",
		CreatedAt: time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
		Latency:   12300 * time.Microsecond,
		User:      "alice",
	}
	os.Stdout.Write(redactsnap.Normalize(redactsnap.Render(e)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "request_id": "<UUID>",
  "created_at": "<TIMESTAMP>",
  "latency": "<DURATION>",
  "user": "alice"
}
```

### Tests

`TestNormalizeRedacts` normalizes one payload and byte-compares against the
golden. `TestNormalizeStableAcrossValues` is the anti-flake proof: it normalizes a
*different* payload (different id, timestamp, and latency) and asserts it yields
the identical golden. `TestRawContainsVolatile` confirms the un-normalized bytes
really do carry the volatile values, so the test is not vacuous.

Create `redact_test.go`:

```go
package redactsnap

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

func TestNormalizeRedacts(t *testing.T) {
	t.Parallel()

	e := Event{
		RequestID: "550e8400-e29b-41d4-a716-446655440000",
		CreatedAt: time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
		Latency:   12300 * time.Microsecond,
		User:      "alice",
	}
	got := Normalize(Render(e))
	want := readGolden(t, "normalized.golden")
	if !bytes.Equal(got, want) {
		t.Fatalf("normalized mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestNormalizeStableAcrossValues(t *testing.T) {
	t.Parallel()

	e := Event{
		RequestID: "11111111-2222-3333-4444-555555555555",
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Latency:   5 * time.Second,
		User:      "alice",
	}
	got := Normalize(Render(e))
	want := readGolden(t, "normalized.golden")
	if !bytes.Equal(got, want) {
		t.Fatalf("different volatile values did not normalize to the same golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRawContainsVolatile(t *testing.T) {
	t.Parallel()

	raw := Render(Event{
		RequestID: "550e8400-e29b-41d4-a716-446655440000",
		CreatedAt: time.Date(2026, 7, 2, 15, 4, 5, 0, time.UTC),
		Latency:   12300 * time.Microsecond,
		User:      "alice",
	})
	if !bytes.Contains(raw, []byte("550e8400")) || !bytes.Contains(raw, []byte("2026-07-02")) {
		t.Fatalf("raw output unexpectedly lacks volatile values, test would be vacuous:\n%s", raw)
	}
}
```

## Review

Normalization is correct when two runs with different volatile values produce the
same bytes — that is precisely what `TestNormalizeStableAcrossValues` asserts, and
it is the property a snapshot needs to stop flaking. The ordering of substitutions
is the subtle trap: run the duration pattern before the timestamp and UUID
patterns and it can match digits inside a timestamp, corrupting the placeholder
form; keep the digit-heavy fields (timestamp, UUID) redacted first. Regex-on-bytes
was the right call here because it needs no knowledge of the key names, but for a
type you own, prefer field-level redaction — unmarshal to a map, overwrite the
named keys — because it cannot accidentally rewrite a real value that merely
resembles a UUID. Run `go test -race` to confirm both payloads collapse to the one
golden and the raw output genuinely carried the volatile data.

## Resources

- [regexp: MustCompile and Regexp.ReplaceAll](https://pkg.go.dev/regexp#Regexp.ReplaceAll) — compiling patterns and replacing every match in a byte slice.
- [time: Time.Format and the RFC3339 layout](https://pkg.go.dev/time#Time.Format) — the timestamp form the normalizer targets.
- [time: Duration.String](https://pkg.go.dev/time#Duration.String) — the human duration form (e.g. `12.3ms`) the duration pattern matches.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-update-flag-golden-file.md](04-update-flag-golden-file.md) | Next: [06-canonical-json-snapshot.md](06-canonical-json-snapshot.md)
