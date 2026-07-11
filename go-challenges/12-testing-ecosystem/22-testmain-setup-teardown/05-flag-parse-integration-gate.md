# Exercise 5: flag.Parse in TestMain and a Custom -integration Flag for Slow/Fast Separation

Backends run a fast unit suite on every commit and a slow integration suite on
demand. The clean way to express "opt in to the slow tests" is a custom
`-integration` flag, declared at package scope and parsed in `TestMain`. This
exercise wires that flag correctly (the common bug is forgetting `flag.Parse()`)
and gates a slow test behind both `-integration` and `testing.Short()`.

This module is fully self-contained: its own `go mod init`, unit-under-test,
demo, and tests. Nothing here imports any other exercise.

## What you'll build

```text
flaggate/                      independent module: example.com/flaggate
  go.mod                       go 1.26
  email.go                     Normalize: trim + lowercase an email address
  cmd/
    demo/
      main.go                  runnable demo: normalize a few addresses
  email_test.go                fast unit test always runs; integration test gated
```

Files: `email.go`, `cmd/demo/main.go`, `email_test.go`.
Implement: `Normalize(email string) string` — a real normalization used before storing or comparing addresses.
Test: declare `var integration = flag.Bool(...)`, call `flag.Parse()` in `TestMain`, run a fast unit test always, and gate a slow test with `if !*integration || testing.Short() { t.Skip(...) }`.
Verify: `go test -count=1 -race ./...` (integration test skips by default; `go test -run Integration -integration` runs it)

Set up the module:

```bash
mkdir -p flaggate/cmd/demo
cd flaggate
go mod init example.com/flaggate
```

### Why flag.Parse must happen in TestMain

`go test` and your test binary share a single flag set. The `-test.*` flags
(including `-short`) are registered by the testing package before `TestMain` runs.
Your custom flag is registered by the package-scope `flag.Bool(...)` initializer.
Neither is *parsed* until something calls `flag.Parse()`. With no custom
`TestMain`, the generated runner parses for you. But once you write your own
`TestMain`, that responsibility is yours: you must call `flag.Parse()` before
`m.Run()`, or `*integration` stays at its default `false` forever and
`-integration` on the command line silently does nothing. This is a frequent,
maddening bug — the flag exists, the code reads it, and it never turns on.

Ordering matters: `flag.Parse()` comes before `m.Run()`, because the tests need
the parsed value. By the time `TestMain` executes, the testing flags are already
registered, so one `flag.Parse()` parses both the framework's flags and yours.

### Two levers: a custom opt-in flag and testing.Short

There are two standard gates and this exercise uses both. `-integration` is an
opt-in: default `false`, so the slow test skips unless you pass `-integration`.
`testing.Short()` is an opt-out for slow work: even with `-integration`, a caller
who passes `-short` (as CI often does for a quick smoke) still skips it. Combining
them, the slow test runs only when the operator explicitly asked for it and did
not ask for a short run:

```go
if !*integration || testing.Short() {
	t.Skip("integration test: pass -integration and omit -short")
}
```

Create `email.go`:

```go
package flaggate

import "strings"

// Normalize canonicalizes an email address for storage and comparison: trim
// surrounding whitespace and lowercase it. Real systems normalize before writing
// so that "Alice@Example.com " and "alice@example.com" are one identity.
func Normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/flaggate"
)

func main() {
	for _, in := range []string{"  Alice@Example.com ", "BOB@site.IO", "carol@x.dev"} {
		fmt.Printf("%q -> %q\n", in, flaggate.Normalize(in))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"  Alice@Example.com " -> "alice@example.com"
"BOB@site.IO" -> "bob@site.io"
"carol@x.dev" -> "carol@x.dev"
```

### Tests

`TestMain` declares nothing itself; the flag is a package-scope var so it registers
during package init. `TestMain` calls `flag.Parse()` then `os.Exit(m.Run())`.
`TestNormalize` is the fast unit test — it always runs. `TestIntegrationRoundTrip`
is the gated slow test — it skips unless `-integration` is set and `-short` is not.

Create `email_test.go`:

```go
package flaggate

import (
	"flag"
	"os"
	"testing"
)

// integration is the opt-in gate for slow tests. It is a package-scope var so it
// is registered before TestMain runs; TestMain parses it.
var integration = flag.Bool("integration", false, "run slow integration tests")

func TestMain(m *testing.M) {
	flag.Parse() // required: our custom flag stays false without this
	os.Exit(m.Run())
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims and lowercases", "  Alice@Example.com ", "alice@example.com"},
		{"already canonical", "carol@x.dev", "carol@x.dev"},
		{"uppercase domain", "BOB@site.IO", "bob@site.io"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIntegrationRoundTrip(t *testing.T) {
	if !*integration || testing.Short() {
		t.Skip("integration test: pass -integration and omit -short")
	}
	// A real integration test would hit external infrastructure here. It is
	// gated so the default `go test ./...` run stays fast and hermetic.
	if Normalize(" X@Y.COM ") != "x@y.com" {
		t.Fatal("normalization regressed under the integration path")
	}
}
```

## Review

The wiring is correct when the flag is a package-scope var and `TestMain` calls
`flag.Parse()` before `m.Run()`; drop that call and `-integration` becomes inert.
The default `go test ./...` runs `TestNormalize` and skips
`TestIntegrationRoundTrip`; `go test -run Integration -integration` runs the gated
test; `go test -short` keeps it skipped even with `-integration`. The unit test is
table-driven and parallel because `Normalize` is pure. The mistake this exercise
targets is the silent one: declaring a flag, reading `*integration` in a test, and
never parsing — the flag looks wired but never fires.

## Resources

- [`flag.Bool` / `flag.Parse`](https://pkg.go.dev/flag#Parse) — declaring and parsing custom flags.
- [`testing`: Main](https://pkg.go.dev/testing#hdr-Main) — why a custom `TestMain` must parse flags itself.
- [`testing.Short`](https://pkg.go.dev/testing#Short) — the `-short` opt-out for slow tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-shared-httptest-harness.md](04-shared-httptest-harness.md) | Next: [06-golden-update-flag-wiring.md](06-golden-update-flag-wiring.md)
