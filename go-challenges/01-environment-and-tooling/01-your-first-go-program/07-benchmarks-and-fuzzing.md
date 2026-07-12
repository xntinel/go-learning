# Exercise 7: Measure and fuzz the parsing hot path

The CLI normalizes every raw URL it is handed before checking it. That helper is a
hot path and a parser, which makes it the natural target for the two robustness
tools of the modern `testing` package: a benchmark using the Go 1.24 `for b.Loop()`
form, and a fuzz target that must never panic. This module builds the helper and
exercises it with both.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
urlnorm/                    independent module: example.com/urlnorm
  go.mod                    go 1.26
  urlnorm.go                Normalize(raw) with ErrEmptyURL / ErrNoHost
  cmd/demo/main.go          runnable demo of normalization
  urlnorm_test.go           table test, BenchmarkNormalize (b.Loop), FuzzNormalize
```

Files: `urlnorm.go`, `cmd/demo/main.go`, `urlnorm_test.go`.
Implement: `Normalize(raw string) (string, error)` that trims, supplies a default
`https` scheme, lowercases scheme and host, and rejects empty or host-less input —
and never panics on any input.
Test: a correctness table; `BenchmarkNormalize` using `for b.Loop()` with
`b.ReportAllocs()`; `FuzzNormalize` with an `f.Add` seed corpus asserting no panic
and idempotence; a `testing.Short()`-gated slow case.
Verify: `go test -count=1 ./...`; `go test -bench=. -benchmem -run='^$'` runs the
benchmark; `go test -run=Fuzz` runs the seed corpus. `gofmt -l` empty.

Set up the module:

```bash
mkdir -p go-solutions/01-environment-and-tooling/01-your-first-go-program/07-benchmarks-and-fuzzing/cmd/demo
cd go-solutions/01-environment-and-tooling/01-your-first-go-program/07-benchmarks-and-fuzzing
```

### The helper, built to never panic

`Normalize` is a pure function: string in, `(string, error)` out. It trims
whitespace, rejects the empty string with `ErrEmptyURL`, supplies a default
`https://` scheme when the input has none, parses with `net/url`, rejects a
host-less result with `ErrNoHost`, and lowercases the scheme and host (which are
case-insensitive per the URL spec) while leaving the path untouched. The single
hard requirement for the fuzz target is that no input — however malformed — makes
it panic; every failure path returns an error instead.

Create `urlnorm.go`:

```go
package urlnorm

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	// ErrEmptyURL is returned for an empty or whitespace-only input.
	ErrEmptyURL = errors.New("url is empty")
	// ErrNoHost is returned when the input parses but has no host.
	ErrNoHost = errors.New("url has no host")
)

// Normalize trims raw, supplies a default https scheme when absent, lowercases
// the scheme and host, and returns the canonical form. It never panics: every
// rejection is an error.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmptyURL
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q: %w", raw, ErrNoHost)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/urlnorm"
)

func main() {
	inputs := []string{"HTTPS://Example.COM/Path", "example.com", "  go.dev  "}
	for _, in := range inputs {
		out, err := urlnorm.Normalize(in)
		if err != nil {
			fmt.Printf("%q -> error: %v\n", in, err)
			continue
		}
		fmt.Printf("%q -> %s\n", in, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"HTTPS://Example.COM/Path" -> https://example.com/Path
"example.com" -> https://example.com
"  go.dev  " -> https://go.dev
```

### Tests, benchmark, and fuzz target

`BenchmarkNormalize` uses `for b.Loop()`: the Go 1.24 form that runs the body an
appropriate number of times while excluding any setup before the loop and managing
the timer, with none of the `b.N`/`b.ResetTimer` bookkeeping the old form needed.
`b.ReportAllocs()` adds allocation counts to the output. `FuzzNormalize` seeds the
corpus with `f.Add` and asserts the two invariants of the helper: it never panics
(reaching the end of `f.Fuzz` without crashing proves that), and it is idempotent
(normalizing an already-normalized value returns the same string). The
`testing.Short()` gate skips the large loop under `go test -short`.

Create `urlnorm_test.go`:

```go
package urlnorm

import (
	"errors"
	"fmt"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "adds scheme", in: "example.com", want: "https://example.com"},
		{name: "lowercases host and scheme", in: "HTTPS://Example.COM/Path", want: "https://example.com/Path"},
		{name: "trims whitespace", in: "  go.dev  ", want: "https://go.dev"},
		{name: "keeps explicit http", in: "http://a.test/x", want: "http://a.test/x"},
		{name: "empty", in: "   ", wantErr: ErrEmptyURL},
		{name: "no host", in: "https://", wantErr: ErrNoHost},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Normalize(%q) err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeIsIdempotentOnManyHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large idempotence sweep in -short mode")
	}
	for i := range 1000 {
		in := fmt.Sprintf("Host-%d.Example.COM/p", i)
		once, err := Normalize(in)
		if err != nil {
			t.Fatalf("Normalize(%q) error: %v", in, err)
		}
		twice, err := Normalize(once)
		if err != nil {
			t.Fatalf("re-normalize %q error: %v", once, err)
		}
		if once != twice {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

func BenchmarkNormalize(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Normalize("HTTPS://Example.COM/a/b/c?q=1")
	}
}

func FuzzNormalize(f *testing.F) {
	f.Add("https://Example.COM/Path")
	f.Add("example.com")
	f.Add("   ")
	f.Add("http://a.test/x")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := Normalize(raw)
		if err != nil {
			return // an error is an acceptable outcome; only a panic is a bug
		}
		again, err := Normalize(got)
		if err != nil {
			t.Fatalf("re-normalizing %q failed: %v", got, err)
		}
		if again != got {
			t.Fatalf("not idempotent: %q -> %q", got, again)
		}
	})
}

func ExampleNormalize() {
	out, _ := Normalize("HTTPS://Example.COM/Path")
	fmt.Println(out)
	// Output: https://example.com/Path
}
```

To run the benchmark and the fuzz seed corpus explicitly:

```bash
go test -bench=. -benchmem -run='^$'   # runs BenchmarkNormalize, measures allocs
go test -run=Fuzz                       # runs the FuzzNormalize seed corpus once
go test -fuzz=FuzzNormalize -fuzztime=5s  # optional: a short active fuzz smoke run
```

## Review

The helper is correct when normalization is total and idempotent: every input
yields either a canonical URL or an error, never a panic, and normalizing a
canonical URL is a no-op. Idempotence is the invariant the fuzz target guards,
because it is the property most likely to break on an input your table never
imagined — a stray percent-encoding, an odd scheme, a host with mixed case you
forgot to lower. The benchmark's `for b.Loop()` is what makes the measurement
honest: setup before the loop is excluded and the timer is handled for you, so the
number reflects the work inside the loop and nothing else.

The traps: do not write a manual `for i := 0; i < b.N; i++` loop with hand-placed
`b.ResetTimer` — `for b.Loop()` supersedes it in Go 1.24+. Do not assert on the
error *text* in the fuzz target; only a panic or a broken invariant is a failure,
and an ordinary error is a valid outcome. Keep the `testing.Short()` gate so
`go test -short` stays fast in a pre-commit hook while CI runs the full sweep.

## Resources

- [testing.B.Loop](https://pkg.go.dev/testing#B.Loop) — the Go 1.24 benchmark loop.
- [Go Fuzzing](https://go.dev/doc/security/fuzz/) — `f.Add`, `f.Fuzz`, the corpus, and running a fuzz target.
- [net/url.Parse](https://pkg.go.dev/net/url#Parse) — parsing and re-serializing a URL.
- [testing.Short](https://pkg.go.dev/testing#Short) — gating slow cases under `-short`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-race-detector-and-test-cache.md](06-race-detector-and-test-cache.md) | Next: [08-build-constraints-cross-compile.md](08-build-constraints-cross-compile.md)
