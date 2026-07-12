# Exercise 8: Golden-File Table Tests for a JSON Serializer

When a function emits a hundred lines of JSON, inlining the expected value in the
test source is unreadable and unreviewable. A golden file moves the expectation
into `testdata/`, where it is diffed in pull requests like any other file. This
module builds `MarshalEvent(Event) ([]byte, error)`, tests it with a table of
`{name, event, golden}`, reads each golden under a `-update` flag, and normalizes
with `json.Indent` so formatting noise never causes a false diff.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests and `testdata/`. It uses `github.com/google/go-cmp`; nothing here imports
another exercise.

## What you'll build

```text
eventenc/                 independent module: example.com/eventenc
  go.mod                  go 1.26; requires github.com/google/go-cmp
  event.go                Event, MarshalEvent (stable JSON envelope)
  testdata/
    user_created.json     golden output for the user_created case
    order_placed.json     golden output for the order_placed case
  cmd/
    demo/
      main.go             marshals an event, prints the JSON
  event_test.go           golden table + -update flag + json.Indent normalization
```

- Files: `event.go`, `testdata/*.json`, `cmd/demo/main.go`, `event_test.go`.
- Implement: `MarshalEvent(Event) ([]byte, error)` producing a stable, indented JSON envelope.
- Test: a table of `{name, event, golden}` that under `-update` rewrites the golden and otherwise reads it, comparing both sides after `json.Indent` normalization; plus a `t.TempDir` round-trip of the write path.
- Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Why golden files, and why normalization

The table here does not carry a `want []byte` column, because a byte-for-byte JSON
expectation the size of a real event envelope would drown the test source. Instead
each row names a `golden` file under `testdata/` — a directory the `go` tool treats
as data, excluding it from build and package resolution, which is exactly why it is
the conventional home for fixtures. The expected value lives in a file a reviewer
can read and diff in a pull request, not buried in a string literal.

The workflow hinges on a package-level flag: `var update = flag.Bool("update",
false, ...)`. Run `go test -update` and each subtest *writes* the produced bytes to
its golden file; run `go test` normally and each subtest *reads* the golden and
compares. The discipline the concepts stress is that regeneration is a deliberate,
reviewed step: you run `-update` when you have intentionally changed the output,
then read the resulting diff in the PR before committing it. A golden test wired to
regenerate on every run asserts nothing.

The normalization is what makes golden JSON robust. Comparing raw bytes would flag
a spurious difference on trailing whitespace or a different indent width — noise,
not a real change. Running both the golden and the produced bytes through
`json.Indent` into a canonical form collapses that noise, so the diff fires only on
a genuine structural or value change. (Key *order* is already stable here because
`encoding/json` sorts map keys and emits struct fields in declaration order, so the
envelope is deterministic before normalization even runs.) The `MarshalEvent`
envelope adds a `version` field and wraps the event's payload, the kind of stable
contract a message bus consumer depends on.

Create `event.go`:

```go
package eventenc

import "encoding/json"

// Event is a domain event to be published to a bus.
type Event struct {
	ID      string
	Type    string
	Source  string
	Payload map[string]any
}

// envelope is the wire form: a versioned wrapper with deterministic field order.
type envelope struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Source  string         `json:"source"`
	Payload map[string]any `json:"payload"`
	Version int            `json:"version"`
}

// MarshalEvent serializes an Event into a stable, indented JSON envelope.
func MarshalEvent(e Event) ([]byte, error) {
	env := envelope{
		ID:      e.ID,
		Type:    e.Type,
		Source:  e.Source,
		Payload: e.Payload,
		Version: 1,
	}
	return json.MarshalIndent(env, "", "  ")
}
```

Now the golden files. In a real project you would generate these with `go test
-update` and commit them; here they are provided so the test has something to read.

Create `testdata/user_created.json`:

```json
{
  "id": "evt-1",
  "type": "user.created",
  "source": "users-api",
  "payload": {
    "email": "alice@example.com",
    "user_id": 42
  },
  "version": 1
}
```

Create `testdata/order_placed.json`:

```json
{
  "id": "evt-2",
  "type": "order.placed",
  "source": "orders-api",
  "payload": {
    "amount_cents": 1999,
    "order_id": "ord-7"
  },
  "version": 1
}
```

### The runnable demo

The demo marshals one event and prints the JSON, so the envelope shape is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/eventenc"
)

func main() {
	b, err := eventenc.MarshalEvent(eventenc.Event{
		ID:      "evt-1",
		Type:    "user.created",
		Source:  "users-api",
		Payload: map[string]any{"user_id": 42, "email": "alice@example.com"},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(b))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": "evt-1",
  "type": "user.created",
  "source": "users-api",
  "payload": {
    "email": "alice@example.com",
    "user_id": 42
  },
  "version": 1
}
```

### The tests

The golden table drives the compare. Under `-update` it writes the golden;
otherwise it reads and diffs after normalizing both sides through `json.Indent`.
`TestUpdateRoundTrip` uses `t.TempDir` to exercise the write path in isolation,
without touching the committed `testdata/`, proving the regeneration path works.

Create `event_test.go`:

```go
package eventenc

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// normalizeJSON reindents bytes to a canonical form so whitespace differences do
// not cause spurious diffs.
func normalizeJSON(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(b), "", "  "); err != nil {
		t.Fatalf("indent %q: %v", b, err)
	}
	return buf.String()
}

func TestMarshalEventGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		event  Event
		golden string
	}{
		{
			name: "user_created",
			event: Event{
				ID:      "evt-1",
				Type:    "user.created",
				Source:  "users-api",
				Payload: map[string]any{"user_id": 42, "email": "alice@example.com"},
			},
			golden: "user_created.json",
		},
		{
			name: "order_placed",
			event: Event{
				ID:      "evt-2",
				Type:    "order.placed",
				Source:  "orders-api",
				Payload: map[string]any{"order_id": "ord-7", "amount_cents": 1999},
			},
			golden: "order_placed.json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := MarshalEvent(tc.event)
			if err != nil {
				t.Fatalf("MarshalEvent(%s) error: %v", tc.name, err)
			}
			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", path, err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
			}
			if diff := cmp.Diff(normalizeJSON(t, want), normalizeJSON(t, got)); diff != "" {
				t.Fatalf("golden mismatch for %s (-want +got):\n%s", tc.name, diff)
			}
		})
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	got, err := MarshalEvent(Event{ID: "x", Type: "t", Source: "s", Payload: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	path := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if diff := cmp.Diff(normalizeJSON(t, got), normalizeJSON(t, back)); diff != "" {
		t.Fatalf("round-trip mismatch (-want +got):\n%s", diff)
	}
}
```

## Review

The serializer is correct when its envelope is deterministic and matches the
committed golden after normalization. The workflow, not the code, is the lesson:
`-update` regenerates, a normal run asserts, and the golden diff is reviewed in the
PR — never regenerated blindly on every run, which would make the assertion a
tautology. The `json.Indent` normalization is what lets the test survive a reindent
or a stray newline without a false failure while still catching a real value or
structure change.

Two details keep the golden honest. `testdata/` is special to the `go` tool, so the
fixtures live there rather than beside the source. And key order is deterministic
because `encoding/json` sorts map keys and preserves struct field order — if your
serializer used a `map` at the top level instead of a struct, you would get the
same stability, but a non-deterministic source (iterating a map into a slice)
would flake, and the golden test is what would expose it.

## Resources

- [encoding/json.MarshalIndent and json.Indent](https://pkg.go.dev/encoding/json#MarshalIndent) — producing and normalizing indented JSON.
- [testing.T.TempDir](https://pkg.go.dev/testing#T.TempDir) — a per-test scratch directory, auto-removed.
- [cmd/go: test packages and testdata](https://pkg.go.dev/cmd/go#hdr-Test_packages) — why `testdata/` is ignored by the build.
- [google/go-cmp](https://pkg.go.dev/github.com/google/go-cmp/cmp) — the diffing tool used here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-cmp-diff-structs.md](09-cmp-diff-structs.md)
