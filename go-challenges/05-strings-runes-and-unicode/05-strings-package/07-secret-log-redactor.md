# Exercise 7: Redact Secret Values From Log Lines With a Single-Pass Replacer

A logging hook must scrub known secret values — a bearer token, a database
password, an API key — out of every log line before it is written. The correct
tool is one `strings.Replacer` built once and shared across request goroutines,
which does a single left-to-right longest-match pass. Chaining `ReplaceAll`
instead re-scans its own output and one replacement can clobber another.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
redactor/                       independent module: example.com/redactor
  go.mod                        go 1.26
  redactor.go                   NewRedactor + Redact (built once, concurrency-safe)
  redactor_test.go              redaction table + single-pass proof + -race concurrency
  cmd/
    demo/
      main.go                   runnable demo
```

Files: `redactor.go`, `redactor_test.go`, `cmd/demo/main.go`.
Implement: `NewRedactor(secrets []string) *Redactor` and
`(*Redactor) Redact(line string) string`.
Test: token and password both redacted, overlapping secrets (longest wins), a
secret that is a substring of the replacement (proves no re-scan), empty secret
list is a no-op, and a `-race` concurrency test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/redactor/cmd/demo
cd ~/go-exercises/redactor
go mod init example.com/redactor
```

### Why one Replacer, and why not chained ReplaceAll

`strings.NewReplacer(pairs...)` replaces in a single left-to-right scan: at each
position it tries the replacement keys and, when several match, applies the one
listed *first* in argument order, then continues *past* the emitted replacement
without re-examining it. Two consequences matter here. First, argument order
decides overlaps: if `abc` is listed before `abcdef`, then in `abcdef` the
scanner matches `abc` first and only `abc` is replaced. To make a longer secret
win over a shorter one that is its prefix, you must list the longer secret first —
so the constructor sorts the secrets by descending length before building the
`Replacer`. Second, no re-scan: the emitted `[REDACTED]` is never matched again,
even if a secret happens to be a substring of the word `REDACTED`. A `*Replacer`
is immutable, so the same pointer is safe to call `Redact` on from every request
goroutine — you build it once in the constructor and share it.

Chained `strings.ReplaceAll` is the buggy alternative. Each call scans the whole
string, including text a previous call produced. If you want the two independent
mappings `foo -> bar` and `bar -> baz`, chaining `ReplaceAll(s, "foo", "bar")`
then `ReplaceAll(_, "bar", "baz")` turns the `bar` you just wrote into `baz` —
the second pass ate the first's output. A single `Replacer` with both mappings
does the right thing because it never revisits emitted text. The redaction case
is the same shape: independent secret-to-placeholder mappings that must not
interfere.

Empty secrets are skipped (an empty key in a `Replacer` is meaningless and would
match at every position), and an empty secret list produces a no-op redactor.

Create `redactor.go`:

```go
package redactor

import (
	"slices"
	"strings"
)

const placeholder = "[REDACTED]"

// Redactor scrubs known secret values from text. It is safe for concurrent use
// by multiple goroutines because the underlying *strings.Replacer is immutable.
type Redactor struct {
	replacer *strings.Replacer
}

// NewRedactor builds a redactor that replaces every non-empty secret with
// placeholder in a single pass. Secrets are sorted longest-first so that a
// longer secret wins over a shorter one that is its prefix (Replacer matches in
// argument order). Build once; share across goroutines.
func NewRedactor(secrets []string) *Redactor {
	sorted := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			sorted = append(sorted, s)
		}
	}
	slices.SortStableFunc(sorted, func(a, b string) int {
		return len(b) - len(a) // longest first
	})
	pairs := make([]string, 0, len(sorted)*2)
	for _, s := range sorted {
		pairs = append(pairs, s, placeholder)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// Redact returns line with every known secret replaced by placeholder.
func (r *Redactor) Redact(line string) string {
	return r.replacer.Replace(line)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/redactor"
)

func main() {
	r := redactor.NewRedactor([]string{"s3cr3t-token", "hunter2"})
	lines := []string{
		"auth ok token=s3cr3t-token user=alice",
		"db connect password=hunter2",
		"nothing secret here",
	}
	for _, line := range lines {
		fmt.Println(r.Redact(line))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
auth ok token=[REDACTED] user=alice
db connect password=[REDACTED]
nothing secret here
```

### Tests

Create `redactor_test.go`:

```go
package redactor

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		secrets []string
		line    string
		want    string
	}{
		{
			name:    "token and password",
			secrets: []string{"tok-abc", "pw-123"},
			line:    "token=tok-abc pass=pw-123",
			want:    "token=[REDACTED] pass=[REDACTED]",
		},
		{
			name:    "overlapping longest wins",
			secrets: []string{"abc", "abcdef"},
			line:    "value=abcdef end",
			want:    "value=[REDACTED] end",
		},
		{
			name:    "no re-scan of placeholder",
			secrets: []string{"RED"}, // substring of the word REDACTED
			line:    "code RED alert",
			want:    "code [REDACTED] alert",
		},
		{
			name:    "empty secret list is a no-op",
			secrets: nil,
			line:    "nothing to redact",
			want:    "nothing to redact",
		},
		{
			name:    "empty secret skipped",
			secrets: []string{"", "key"},
			line:    "key=key",
			want:    "[REDACTED]=[REDACTED]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRedactor(tc.secrets)
			if got := r.Redact(tc.line); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestChainedReplaceAllIsBuggy documents why a single Replacer is required: two
// chained ReplaceAll passes clobber each other, a single Replacer does not.
func TestChainedReplaceAllIsBuggy(t *testing.T) {
	t.Parallel()

	in := "foo bar"

	chained := strings.ReplaceAll(in, "foo", "bar")
	chained = strings.ReplaceAll(chained, "bar", "baz")
	if chained != "baz baz" {
		t.Fatalf("chained ReplaceAll = %q, want the buggy %q", chained, "baz baz")
	}

	onePass := strings.NewReplacer("foo", "bar", "bar", "baz").Replace(in)
	if onePass != "bar baz" {
		t.Fatalf("single Replacer = %q, want the correct %q", onePass, "bar baz")
	}
}

func TestRedactConcurrent(t *testing.T) {
	t.Parallel()

	r := NewRedactor([]string{"secret"})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := r.Redact(fmt.Sprintf("req %d value=secret", i))
			if !strings.Contains(got, "[REDACTED]") {
				t.Errorf("goroutine %d: %q not redacted", i, got)
			}
		}()
	}
	wg.Wait()
}

func ExampleRedactor_Redact() {
	r := NewRedactor([]string{"hunter2"})
	fmt.Println(r.Redact("password=hunter2"))
	// Output: password=[REDACTED]
}
```

## Review

The redactor is correct when overlapping secrets resolve longest-first and the
emitted `[REDACTED]` is never re-scanned — the `RED`-inside-`REDACTED` case
proves the single pass. `TestChainedReplaceAllIsBuggy` is the contrast that
justifies the design: chaining `ReplaceAll` produces `baz baz` where a single
`Replacer` produces `bar baz`. The concurrency test under `-race` proves the
`*Replacer` is safe to share, which is why it is built once in the constructor,
not per call. Confirm with `go test -race`. Note this scrubs known *values*; it
does not find secrets by pattern — that is a regexp job and a different tool.

## Resources

- [strings.NewReplacer](https://pkg.go.dev/strings#NewReplacer) and [strings.Replacer](https://pkg.go.dev/strings#Replacer).
- [strings.ReplaceAll](https://pkg.go.dev/strings#ReplaceAll) — the multi-pass contrast.
- [Go Blog: strings, bytes, runes and characters](https://go.dev/blog/strings) — how the package treats text.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-url-path-prefix-router.md](06-url-path-prefix-router.md) | Next: [08-case-insensitive-header-lookup.md](08-case-insensitive-header-lookup.md)
