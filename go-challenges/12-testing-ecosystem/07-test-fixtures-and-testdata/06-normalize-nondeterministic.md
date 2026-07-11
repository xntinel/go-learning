# Exercise 6: Normalize volatile fields before golden comparison

The single most common reason a golden test flakes: the output embeds a timestamp, a UUID, or a duration that changes every run, so `bytes.Equal` against a committed golden fails on data that was never wrong. The fix is a normalization pass that replaces the volatile fields with stable placeholders on both sides before comparing. This module serializes an audit event and normalizes it for a deterministic golden.

## What you'll build

```text
audit/                        independent module: example.com/auditgold
  go.mod                      go 1.26
  audit.go                    AuditEvent, NewAuditEvent, Serialize, Normalize
  cmd/
    demo/
      main.go                 serializes an event and prints its normalized form
  audit_test.go               golden compare of normalized output; negative + stability tests
  testdata/
    audit.golden              the normalized expected JSON
```

Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`, `testdata/audit.golden`.
Implement: `AuditEvent` with volatile fields (id, timestamp, duration), `Serialize` to indented JSON, and `Normalize` replacing RFC3339 timestamps, UUIDs, and the duration value with `<ts>`/`<uuid>`/`<dur>`.
Test: serialize a fresh event, `Normalize`, and `bytes.Equal`-compare to `testdata/audit.golden`; a negative test proves a changed actor/action still fails; a stability test proves two runs normalize identically.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/audit/cmd/demo ~/go-exercises/audit/testdata
cd ~/go-exercises/audit
go mod init example.com/auditgold
```

### The golden-file tax: determinism

A golden test asserts byte equality against a committed reference. That only works if the output is deterministic. Serialized audit events, request logs, and event envelopes almost never are: they carry an `id` (a fresh UUID per event), a `timestamp` (wall clock), and a `duration_ms` (however long the operation happened to take). Compare that raw output to a golden and it fails every run on fields that were correct — the test is red for noise, and a suite that is red for noise trains the team to ignore red, which is strictly worse than having no test.

Normalization is the disciplined fix. Before comparing, run both the produced output and the golden through a pass that replaces each volatile field with a fixed token: every UUID becomes `<uuid>`, every RFC3339 timestamp becomes `<ts>`, the duration value becomes `<dur>`. What remains — the actor, the action, the structure, the field names — is exactly the part you actually want to assert, and it is stable. The golden stores the already-normalized form, so a review of `audit.golden` shows the meaningful shape without a single volatile value.

The knife-edge is over-normalization. A regex broad enough to blank out a field you care about would let a real regression pass silently. The negative test in this module exists precisely to prove the boundary: it changes `actor` and `action` and asserts the normalized output *still* differs from the golden. If that test ever passes-through (i.e. the changed event matches the golden), the normalization is masking real content and must be tightened. Normalize the volatile, never the asserted.

The regexes match narrowly. The UUID pattern is the canonical 8-4-4-4-12 hex; the timestamp pattern is RFC3339 with optional fractional seconds and a `Z` or numeric offset; the duration pattern matches only the `"duration_ms":` numeric value, leaving the key intact.

Create `audit.go`:

```go
package auditgold

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// AuditEvent is a serialized audit record. id, timestamp, and duration are
// volatile: they change every run and must be normalized before a golden compare.
type AuditEvent struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	DurationMS int64     `json:"duration_ms"`
}

// NewAuditEvent fills the volatile fields with live values.
func NewAuditEvent(actor, action string, d time.Duration) AuditEvent {
	return AuditEvent{
		ID:         newID(),
		Timestamp:  time.Now().UTC(),
		Actor:      actor,
		Action:     action,
		DurationMS: d.Milliseconds(),
	}
}

// Serialize renders the event as indented JSON.
func (e AuditEvent) Serialize() ([]byte, error) {
	return json.MarshalIndent(e, "", "  ")
}

var (
	uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tsRe   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	durRe  = regexp.MustCompile(`"duration_ms":\s*\d+`)
)

// Normalize replaces volatile fields with stable placeholders so the output can
// be compared to a committed golden. It touches only id, timestamp, and the
// duration value; every other field is left exactly as serialized.
func Normalize(b []byte) []byte {
	b = uuidRe.ReplaceAll(b, []byte("<uuid>"))
	b = tsRe.ReplaceAll(b, []byte("<ts>"))
	b = durRe.ReplaceAll(b, []byte(`"duration_ms": <dur>`))
	return b
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```

The golden stores the normalized shape. Note the placeholders where volatile values would be.

Create `testdata/audit.golden`:

```text
{
  "id": "<uuid>",
  "timestamp": "<ts>",
  "actor": "alice",
  "action": "secret.read",
  "duration_ms": <dur>
}
```

### The runnable demo

Because the demo prints the *normalized* output, its output is deterministic even though the underlying event carries a fresh UUID, the current time, and a duration.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/auditgold"
)

func main() {
	ev := auditgold.NewAuditEvent("alice", "secret.read", 1234*time.Millisecond)
	raw, err := ev.Serialize()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(auditgold.Normalize(raw)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "<uuid>",
  "timestamp": "<ts>",
  "actor": "alice",
  "action": "secret.read",
  "duration_ms": <dur>
}
```

### The test

Three tests pin the contract. The golden test normalizes a fresh event and compares. The negative test changes the asserted fields and proves the normalized output still differs from the golden — normalization is not masking content. The stability test normalizes two independently-constructed events (different UUIDs, timestamps, durations) and proves they collapse to the same bytes, which is the whole point of the pass.

Create `audit_test.go`:

```go
package auditgold

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustNormalized(t *testing.T, ev AuditEvent) []byte {
	t.Helper()
	raw, err := ev.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return bytes.TrimSpace(Normalize(raw))
}

func goldenBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "audit.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return bytes.TrimSpace(b)
}

func TestAuditGolden(t *testing.T) {
	t.Parallel()

	got := mustNormalized(t, NewAuditEvent("alice", "secret.read", 1234*time.Millisecond))
	if !bytes.Equal(got, goldenBytes(t)) {
		t.Fatalf("normalized mismatch:\ngot:\n%s\nwant:\n%s", got, goldenBytes(t))
	}
}

func TestNormalizeDoesNotMaskFieldChange(t *testing.T) {
	t.Parallel()

	got := mustNormalized(t, NewAuditEvent("mallory", "secret.delete", 9*time.Millisecond))
	if bytes.Equal(got, goldenBytes(t)) {
		t.Fatal("normalization masked a real actor/action change")
	}
}

func TestNormalizeStableAcrossRuns(t *testing.T) {
	t.Parallel()

	a := mustNormalized(t, NewAuditEvent("alice", "secret.read", 1*time.Millisecond))
	b := mustNormalized(t, NewAuditEvent("alice", "secret.read", 5000*time.Millisecond))
	if !bytes.Equal(a, b) {
		t.Fatalf("normalized output not stable across volatile differences:\n%s\nvs\n%s", a, b)
	}
}
```

## Review

The golden is trustworthy when normalization erases exactly the volatile fields and nothing else. The two failure poles are the ones to hold in mind: too little normalization and the test flakes on timestamps and UUIDs, teaching the team to ignore it; too much and a genuine regression in `actor` or `action` slips through a placeholder. The negative test is the guardrail against the second — keep it, and keep it failing when it should. Normalize both sides identically, store the normalized form as the golden, and reach for a canonical serializer (next exercise) when the drift is structural rather than value-level.

## Resources

- [regexp: Regexp.ReplaceAll](https://pkg.go.dev/regexp#Regexp.ReplaceAll) — byte-slice replacement of volatile spans.
- [encoding/json: MarshalIndent](https://pkg.go.dev/encoding/json#MarshalIndent) — deterministic indented serialization.
- [RFC 3339](https://www.rfc-editor.org/rfc/rfc3339) — the timestamp format the normalizer matches.

---

Back to [05-embed-fixtures.md](05-embed-fixtures.md) | Next: [07-test-data-builder.md](07-test-data-builder.md)
