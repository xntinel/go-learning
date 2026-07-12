# Exercise 5: Expanding the Linter Set Deliberately (gocritic)

Adding a linter is a team decision, not a reflex. This exercise takes a small HTTP
status classifier, adds `gocritic` to the enable list, reads the `golangci-lint
linters` catalog to see what is enabled versus available, runs it, and evaluates
whether the new findings earn their keep — the start-small-expand-carefully
discipline in miniature.

This module is self-contained: its own `go mod init`, a `httpstatus` package, a
demo, and a table test.

## What you'll build

```text
statusclass/                  independent module: example.com/statusclass
  go.mod                      go 1.24
  httpstatus.go               Classify(code int) Class; String on Class
  httpstatus_test.go          table test over the class boundaries
  cmd/
    demo/
      main.go                 classifies a handful of codes
  .golangci.yml               enable list + gocritic (shown in prose)
```

- Files: `httpstatus.go`, `httpstatus_test.go`, `cmd/demo/main.go`, plus the config in prose.
- Implement: `Classify(code int) Class` mapping an HTTP status code to a category, and a `String()` on `Class`.
- Test: a table over representative codes and each boundary.
- Verify: `go test -count=1 -race ./...`; then `golangci-lint linters` and `golangci-lint run ./...`.

### Reading the linter catalog

Before adding a linter, look at what is already on and what is available:

```bash
golangci-lint linters
```

The output has two sections — "Enabled by your configuration linters:" and
"Disabled by your configuration linters:" — each linter tagged with the bug classes
it covers (bugs, error, style, performance, ...). This is how you shop for the next
linter deliberately: you read what a candidate actually checks before turning it on,
rather than enabling `default: all` and drowning. `gocritic` is a large bundle of
opinionated checks spanning style, performance, and diagnostic categories, which is
exactly why it is a good case study in "is this worth the noise for us".

### The code gocritic would comment on

A naive classifier written as an if/else-if chain is something `gocritic`'s
`ifElseChain` check flags, suggesting a `switch`:

```go
func Classify(code int) Class {
	if code >= 100 && code < 200 {
		return Informational
	} else if code >= 200 && code < 300 {
		return Success
	} else if code >= 300 && code < 400 {
		return Redirection
	} else if code >= 400 && code < 500 {
		return ClientError
	} else {
		return ServerError
	}
}
```

That is a real, common finding: the chain is harder to scan than a `switch`, and the
final `else` silently classifies `600` or `-1` as a server error. Taking the finding
seriously produces the clearer version below, which switches on the status *class*
and returns `Unknown` for out-of-range codes. `gocritic` earned its keep here by
nudging the code toward a form that also fixed a latent correctness gap.

Create `httpstatus.go`:

```go
package httpstatus

// Class is a coarse category of HTTP status code.
type Class int

const (
	Unknown Class = iota
	Informational
	Success
	Redirection
	ClientError
	ServerError
)

// String returns a stable, lowercase label for the class.
func (c Class) String() string {
	switch c {
	case Informational:
		return "informational"
	case Success:
		return "success"
	case Redirection:
		return "redirection"
	case ClientError:
		return "client-error"
	case ServerError:
		return "server-error"
	default:
		return "unknown"
	}
}

// Classify maps an HTTP status code to its class. Codes outside 100..599
// return Unknown rather than being lumped into an else branch.
func Classify(code int) Class {
	switch {
	case code >= 100 && code < 200:
		return Informational
	case code >= 200 && code < 300:
		return Success
	case code >= 300 && code < 400:
		return Redirection
	case code >= 400 && code < 500:
		return ClientError
	case code >= 500 && code < 600:
		return ServerError
	default:
		return Unknown
	}
}
```

### The config

Start from the enable list and add `gocritic`:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - staticcheck
    - unused
    - ineffassign
    - bodyclose
    - gocritic
  settings:
    gocritic:
      enabled-tags:
        - diagnostic
        - performance

run:
  timeout: 5m
```

`linters.settings.gocritic` lets you scope the bundle: `enabled-tags` turns on whole
categories (here `diagnostic` and `performance`, the highest-signal ones) while
leaving the noisier `style` and `opinionated` tags off. This is the deliberate part
— you do not have to accept all of `gocritic`; you pick the checks that pay for
themselves. Run it and see:

```bash
golangci-lint run ./...
```

With the `switch`-based code above, `gocritic` is clean. Reintroduce the if/else
chain and it reports the `ifElseChain` finding, giving the team a concrete artifact
to evaluate: keep the check (and fix the code) or scope it out. Removing `gocritic`
from the enable list returns the module to the previous gate — adding a linter is an
explicit, reversible choice, which is exactly the property you want.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/statusclass"
)

func main() {
	for _, code := range []int{200, 301, 404, 503, 600} {
		fmt.Printf("%d -> %s\n", code, httpstatus.Classify(code))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 -> success
301 -> redirection
404 -> client-error
503 -> server-error
600 -> unknown
```

### Tests

Create `httpstatus_test.go`. The table covers one representative code per class and
the exact boundaries, including out-of-range codes that must return `Unknown`.

```go
package httpstatus

import "testing"

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want Class
	}{
		{code: 100, want: Informational},
		{code: 199, want: Informational},
		{code: 200, want: Success},
		{code: 299, want: Success},
		{code: 301, want: Redirection},
		{code: 404, want: ClientError},
		{code: 503, want: ServerError},
		{code: 99, want: Unknown},
		{code: 600, want: Unknown},
		{code: -1, want: Unknown},
	}

	for _, tc := range tests {
		if got := Classify(tc.code); got != tc.want {
			t.Errorf("Classify(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestClassString(t *testing.T) {
	t.Parallel()

	if got := Success.String(); got != "success" {
		t.Errorf("Success.String() = %q, want %q", got, "success")
	}
	if got := Class(999).String(); got != "unknown" {
		t.Errorf("Class(999).String() = %q, want %q", got, "unknown")
	}
}
```

## Review

The classifier is correct when every code maps to exactly one class and everything
outside 100..599 returns `Unknown` — the boundary and out-of-range cases in the
table are what prove the `switch` did not reintroduce the if/else chain's habit of
lumping garbage into the last branch. The `gocritic` story is the real lesson:
adding a linter is a scoped, reversible decision made by reading the catalog and
choosing tags (`diagnostic`, `performance`) rather than swallowing the whole bundle,
and its `ifElseChain` finding is worth keeping precisely because taking it seriously
also fixed a correctness gap. The mistakes to avoid: enabling `default: all` and
treating the flood as normal, accepting all of `gocritic` without scoping tags (the
`opinionated` and `style` checks are where teams disagree), and forgetting that a
linter you add can be removed — the gate should reflect decisions the team actually
made.

## Resources

- [golangci-lint: linters command](https://golangci-lint.run/docs/configuration/cli/) — listing enabled versus available linters.
- [gocritic](https://go-critic.com/) — the check catalog, organized by tag.
- [golangci-lint: gocritic settings](https://golangci-lint.run/docs/linters/configuration/) — `enabled-tags`, `enabled-checks`, `disabled-tags`.

---

Back to [04-reproducible-golangci-config-v2.md](04-reproducible-golangci-config-v2.md) | Next: [06-migrate-v1-config-to-v2.md](06-migrate-v1-config-to-v2.md)
