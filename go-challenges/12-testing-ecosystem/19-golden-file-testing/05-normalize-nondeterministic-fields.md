# Exercise 5: Normalizing Timestamps, UUIDs, and Durations

A response envelope carries fields that change every run — a `created_at`
timestamp, a `request_id` UUID, an `elapsed_ms` duration — and a golden over its
raw bytes would flake immediately. You build an envelope and make it
golden-stable two ways: regexp redaction of the raw bytes, and
`cmpopts.IgnoreFields` on the decoded struct.

This module imports `github.com/google/go-cmp`. It is otherwise fully
self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
envelope/                  independent module: example.com/envelope
  go.mod                   go 1.26; requires github.com/google/go-cmp
  envelope.go              Envelope, BuildEnvelope, Normalize (regexp redaction)
  testdata/
    envelope.golden        normalized reference (placeholders, not live values)
  cmd/
    demo/
      main.go              builds an envelope and prints its normalized form
  envelope_test.go         determinism, golden-after-normalize, IgnoreFields
```

Files: `envelope.go`, `testdata/envelope.golden`, `cmd/demo/main.go`, `envelope_test.go`.
Implement: `BuildEnvelope(map[string]string) ([]byte, error)` (with a live timestamp, UUID, and measured duration) and `Normalize([]byte) []byte`.
Test: prove `Normalize` is deterministic across runs, compare the normalized bytes to the golden, and separately ignore the volatile fields with `cmpopts.IgnoreFields` on the decoded struct.
Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
mkdir -p ~/go-exercises/envelope/cmd/demo ~/go-exercises/envelope/testdata
cd ~/go-exercises/envelope
go mod init example.com/envelope
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Two ways to remove non-determinism at the comparison boundary

The envelope embeds three genuinely volatile fields, and the only way to golden
it is to make those fields not matter. There are two techniques, and this
exercise shows both because they suit different situations.

The first is *regexp redaction of the raw bytes*: `Normalize` runs the serialized
output through a small set of regular expressions that replace a UUID pattern
with `<uuid>`, an RFC 3339 timestamp with `<timestamp>`, and the numeric duration
with `0`. Both the actual output and the committed golden are in the redacted
form, so the byte compare passes as long as the *structure and stable fields*
match. This is the right tool when you do not control the type — logs, a
third-party payload, anything you only have as bytes — and it operates purely on
text.

The second is `cmpopts.IgnoreFields` on the *decoded struct*: unmarshal the raw
bytes into `Envelope`, then `cmp.Diff(want, got,
cmpopts.IgnoreFields(Envelope{}, "RequestID", "CreatedAt", "ElapsedMS"))` compares
every field except the three volatile ones, which are dropped by name. This is
cleaner when you own the type, because it names typed fields instead of matching
text patterns, and it cannot accidentally redact a stable field that happens to
look like a UUID.

Both share one danger: over-normalization. Every field you scrub or ignore is a
field the golden no longer pins, so a real regression in that field passes
silently. Scrub the minimum — the three fields that are actually
non-deterministic — and keep everything else exact. A `Normalize` that redacted
the whole `data` object would make the test green and worthless.

The determinism proof is the key test: `BuildEnvelope` produces different bytes
every call (new UUID, new timestamp, new measured duration), yet
`Normalize(build())` must be byte-identical across two calls. If it is not, a
volatile field escaped the redaction and the golden would flake.

Create `envelope.go`:

```go
package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Envelope wraps a response payload with observability metadata. RequestID,
// CreatedAt, and ElapsedMS are non-deterministic and must be normalized before a
// golden comparison.
type Envelope struct {
	RequestID string            `json:"request_id"`
	CreatedAt time.Time         `json:"created_at"`
	ElapsedMS int64             `json:"elapsed_ms"`
	Data      map[string]string `json:"data"`
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// BuildEnvelope produces a live envelope with a fresh id, timestamp, and a
// measured duration. Its raw bytes differ on every call.
func BuildEnvelope(data map[string]string) ([]byte, error) {
	start := time.Now()
	env := Envelope{
		RequestID: newUUID(),
		CreatedAt: time.Now().UTC(),
		ElapsedMS: time.Since(start).Milliseconds(),
		Data:      data,
	}
	return json.MarshalIndent(env, "", "  ")
}

var (
	reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reTime = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	reMS   = regexp.MustCompile(`"elapsed_ms": \d+`)
)

// Normalize redacts the three volatile fields to fixed placeholders and applies
// a single-trailing-newline policy, producing a golden-stable form.
func Normalize(b []byte) []byte {
	b = reUUID.ReplaceAll(b, []byte("<uuid>"))
	b = reTime.ReplaceAll(b, []byte("<timestamp>"))
	b = reMS.ReplaceAll(b, []byte(`"elapsed_ms": 0`))
	return append(bytes.TrimRight(b, "\n"), '\n')
}
```

Now the normalized golden — placeholders, never live values.

Create `testdata/envelope.golden`:

```text
{
  "request_id": "<uuid>",
  "created_at": "<timestamp>",
  "elapsed_ms": 0,
  "data": {
    "status": "ok",
    "user": "ada"
  }
}
```

### The runnable demo

The demo prints the *normalized* envelope so the output is deterministic; the raw
envelope would differ on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/envelope"
)

func main() {
	raw, err := envelope.BuildEnvelope(map[string]string{"status": "ok", "user": "ada"})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Print(string(envelope.Normalize(raw)))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "request_id": "<uuid>",
  "created_at": "<timestamp>",
  "elapsed_ms": 0,
  "data": {
    "status": "ok",
    "user": "ada"
  }
}
```

### Tests

Create `envelope_test.go`:

```go
package envelope

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

func TestNormalizeDeterministic(t *testing.T) {
	t.Parallel()
	a, err := BuildEnvelope(map[string]string{"status": "ok", "user": "ada"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := BuildEnvelope(map[string]string{"status": "ok", "user": "ada"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("raw envelopes should differ (volatile fields); they were identical")
	}
	if !bytes.Equal(Normalize(a), Normalize(b)) {
		t.Fatalf("normalized output not deterministic:\n%s\nvs\n%s", Normalize(a), Normalize(b))
	}
}

func TestEnvelopeGoldenNormalized(t *testing.T) {
	raw, err := BuildEnvelope(map[string]string{"status": "ok", "user": "ada"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := Normalize(raw)
	path := filepath.Join("testdata", "envelope.golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s (run: go test -update)\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestIgnoreFieldsSemantic(t *testing.T) {
	t.Parallel()
	raw, err := BuildEnvelope(map[string]string{"status": "ok", "user": "ada"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got Envelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := Envelope{Data: map[string]string{"status": "ok", "user": "ada"}}
	opts := cmpopts.IgnoreFields(Envelope{}, "RequestID", "CreatedAt", "ElapsedMS")
	if diff := cmp.Diff(want, got, opts); diff != "" {
		t.Errorf("envelope mismatch on stable fields (-want +got):\n%s", diff)
	}
}

func ExampleNormalize() {
	in := []byte(`{"request_id": "fdfbe27f-78d1-41a4-92ef-c60962d72428"}`)
	os.Stdout.Write(Normalize(in))
	// Output: {"request_id": "<uuid>"}
}
```

## Review

The suite is correct when `TestNormalizeDeterministic` proves two live envelopes
differ in raw bytes but collapse to identical normalized bytes — that is the whole
guarantee that the golden will not flake. The two techniques are complementary:
regexp redaction works on bytes you do not own; `IgnoreFields` works on a type you
do. The failure this exercise trains against is over-normalization: every scrubbed
field stops being pinned, so scrub only the three that are actually
non-deterministic and leave `data` exact — a test that redacts everything is green
and meaningless. Prefer removing non-determinism at the source (a fixed clock and
id generator) when you control the code; reach for normalization when you only have
the output.

## Resources

- [cmpopts.IgnoreFields](https://pkg.go.dev/github.com/google/go-cmp/cmp/cmpopts#IgnoreFields) — dropping named fields from a structured compare.
- [regexp.Regexp.ReplaceAll](https://pkg.go.dev/regexp#Regexp.ReplaceAll) — redacting volatile patterns in raw bytes.
- [time.Time (RFC 3339)](https://pkg.go.dev/time#Time.MarshalJSON) — how a timestamp serializes, and why it is volatile.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-json-marshalindent-snapshot.md](06-json-marshalindent-snapshot.md)
