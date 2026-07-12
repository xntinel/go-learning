# Exercise 6: Wire a Package -update Flag Through TestMain for Golden-File Regeneration

Golden-file tests compare a function's output against committed reference bytes.
The real workflow needs a switch to *regenerate* those references when the output
legitimately changes. The standard switch is a package-level `-update` flag parsed
in `TestMain`: without it, tests compare; with it, tests rewrite the golden files.
This exercise wires that flag and builds the `assertGolden` helper around it.

This module is fully self-contained: its own `go mod init`, serializer, golden
fixture, demo, and tests. Nothing here imports any other exercise.

## What you'll build

```text
goldenflag/                    independent module: example.com/goldenflag
  go.mod                       go 1.26
  render.go                    Render: serialize a User to pretty JSON bytes
  testdata/
    user.golden                committed reference output
  cmd/
    demo/
      main.go                  runnable demo: print the rendered JSON
  render_test.go               -update flag; assertGolden compares or rewrites
```

Files: `render.go`, `testdata/user.golden`, `cmd/demo/main.go`, `render_test.go`.
Implement: `Render(u User) []byte` — deterministic pretty-JSON serialization.
Test: declare `var update = flag.Bool("update", ...)`, `flag.Parse()` in `TestMain`; `assertGolden(t, name, got)` writes when `*update`, else reads and compares with `bytes.Equal`.
Verify: `go test -count=1 -race ./...` (compares); `go test -update` (rewrites golden)

### Why a flag, and why it must never run in CI

A golden test is only useful if regenerating the golden is one command, not a
manual copy-paste. So the helper has two modes selected by `-update`. In compare
mode it reads `testdata/<name>` and fails on any byte difference — that is the
regression guard. In update mode it *writes* the current output to
`testdata/<name>` and returns without asserting — that is the approval step, run
by a human who has reviewed the diff and accepts the new output.

The sharp edge: `-update` makes every golden test pass unconditionally, because it
overwrites the reference with whatever the code produced. So `-update` must never
run in CI. It is a local, deliberate, human-in-the-loop action; the committed
golden file is the source of truth that CI checks against. Wiring the flag through
`TestMain` (rather than reading an env var ad hoc) keeps it a first-class,
discoverable test flag.

### Deterministic output is a precondition

Golden comparison is byte-exact, so `Render` must be deterministic: stable field
order (struct field order under `json.MarshalIndent`), no timestamps, no map
iteration (map key order is randomized in Go and would flake a golden). This
`Render` serializes a struct with `MarshalIndent` and appends a trailing newline
so the golden file is newline-terminated like a normal text file.

Create `render.go`:

```go
package goldenflag

import "encoding/json"

// User is the payload we render to golden-checked JSON.
type User struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	Email string   `json:"email"`
	Roles []string `json:"roles"`
}

// Render serializes u to indented JSON with a trailing newline. Output is
// deterministic: struct field order is stable and there are no maps or times, so
// the bytes are safe to compare against a committed golden file.
func Render(u User) []byte {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		// User has only JSON-safe fields; marshaling cannot fail.
		panic(err)
	}
	return append(b, '\n')
}
```

The committed reference is the exact bytes `Render` produces for the fixture user.

Create `testdata/user.golden`:

```json
{
  "id": 7,
  "name": "Ada Lovelace",
  "email": "ada@example.com",
  "roles": [
    "admin",
    "engineer"
  ]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/goldenflag"
)

func main() {
	u := goldenflag.User{
		ID:    7,
		Name:  "Ada Lovelace",
		Email: "ada@example.com",
		Roles: []string{"admin", "engineer"},
	}
	os.Stdout.Write(goldenflag.Render(u))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "id": 7,
  "name": "Ada Lovelace",
  "email": "ada@example.com",
  "roles": [
    "admin",
    "engineer"
  ]
}
```

### Tests

`TestMain` parses the flags (so `-update` is honored) and exits with the code.
`assertGolden` is the helper: in update mode it writes the golden and returns; in
compare mode it reads the golden and fails on mismatch, printing both sides.
`TestUserGolden` renders the fixture and asserts it against `testdata/user.golden`.

Create `render_test.go`:

```go
package goldenflag

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

// assertGolden compares got against testdata/<name>, or rewrites it under -update.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with: go test -update)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch for %s\n got:  %q\n want: %q", name, got, want)
	}
}

func TestUserGolden(t *testing.T) {
	u := User{
		ID:    7,
		Name:  "Ada Lovelace",
		Email: "ada@example.com",
		Roles: []string{"admin", "engineer"},
	}
	assertGolden(t, "user.golden", Render(u))
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	u := User{ID: 1, Name: "x", Email: "x@y.z", Roles: []string{"a"}}
	first := Render(u)
	for range 5 {
		if got := Render(u); !bytes.Equal(got, first) {
			t.Fatalf("Render not deterministic: %q vs %q", got, first)
		}
	}
}
```

## Review

The wiring is correct when `-update` is a package-scope flag parsed in `TestMain`,
and `assertGolden` branches on `*update`: rewrite-and-return versus read-and-
compare. `go test` compares against the committed `testdata/user.golden`;
`go test -update` regenerates it, and re-running without `-update` then passes.
`Render` must stay deterministic — `TestRenderIsDeterministic` guards that, since
a map field or a timestamp would make the golden flaky. The rule to internalize:
`-update` makes any golden test pass, so it is a local approval action and must
never run in CI, where the committed golden is the ground truth.

## Resources

- [`encoding/json.MarshalIndent`](https://pkg.go.dev/encoding/json#MarshalIndent) — deterministic pretty-printing for golden output.
- [`flag.Bool` / `flag.Parse`](https://pkg.go.dev/flag#Parse) — the `-update` flag, parsed in `TestMain`.
- [Go blog: Testdata and golden files (subtests article)](https://go.dev/blog/subtests) — the `-update` golden-file idiom in practice.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-flag-parse-integration-gate.md](05-flag-parse-integration-gate.md) | Next: [07-env-timezone-global-restore.md](07-env-timezone-global-restore.md)
