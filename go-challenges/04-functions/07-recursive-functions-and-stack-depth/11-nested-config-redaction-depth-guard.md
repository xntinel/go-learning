# Exercise 11: Redact Secrets in a Nested Config Map with a Depth Guard

**Nivel: Intermedio** — validacion rapida (un test corto).

A config blob decoded from JSON into `map[string]any` and `[]any` can nest a
password or a token at any depth — under `database`, inside an array of
tokens, wherever a plugin author put it. This module recurses over the
decoded structure and replaces every sensitive value with a placeholder, no
matter how deep, while still refusing to descend past a hard depth cap.

This module is fully self-contained: its own `go mod init`, the redactor and
the tests inline.

## What you'll build

```text
configredact/               independent module: example.com/configredact
  go.mod                     go 1.24
  redact.go                  const Placeholder; func Redact
  redact_test.go              nested redaction, depth guard, scalar passthrough
```

Files: `redact.go`, `redact_test.go`.
Implement: `func Redact(v any, maxDepth int) (any, error)` that walks
`map[string]any`, `[]any`, and scalars, replacing the value of any key in
`{password, secret, token, apikey}` (case-insensitive) with `Placeholder`.
Test: a nested map and slice with sensitive keys at different depths compared
with `reflect.DeepEqual`, a depth guard at exactly the cap and one past it,
and a bare scalar passing through unchanged.
Verify: `go test -count=1 ./...`

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/11-nested-config-redaction-depth-guard
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/11-nested-config-redaction-depth-guard
go mod edit -go=1.24
```

### Why this guard exists even though the JSON exercise already streams

The earlier untrusted-JSON exercise in this lesson defends the *decode*: it
streams tokens and counts nesting before anything is materialized, because
that is where an attacker's oversized payload does its damage. By the time
`Redact` runs, the config is already a fully decoded `any` tree sitting in
memory — the primary defense already happened, or the config came from a
plugin author who is careless rather than hostile. The depth guard here is
defense in depth: a second, independent bound so a redaction pass over
already-materialized data cannot itself run away on a pathological structure.

Create `redact.go`:

```go
package redact

import (
	"errors"
	"fmt"
	"strings"
)

// ErrMaxDepthExceeded is returned when the input nests deeper than maxDepth.
// A config blob loaded from a plugin or a third-party integration is already
// fully materialized in memory by the time Redact sees it, so this guard is
// defense in depth, not the primary defense (streaming, as in the JSON depth
// guard exercise, is the primary defense before materializing untrusted input).
var ErrMaxDepthExceeded = errors.New("redact: max depth exceeded")

// Placeholder replaces the value of any key considered sensitive.
const Placeholder = "***REDACTED***"

var sensitiveKeys = map[string]bool{
	"password": true,
	"secret":   true,
	"token":    true,
	"apikey":   true,
}

// Redact walks v (the decoded result of an arbitrary JSON-like document —
// map[string]any, []any, or a scalar) and returns a copy with every value
// under a sensitive key replaced by Placeholder, at any depth. It refuses to
// descend past maxDepth.
func Redact(v any, maxDepth int) (any, error) {
	return redact(v, 0, maxDepth)
}

func redact(v any, depth int, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("depth %d: %w", depth, ErrMaxDepthExceeded)
	}

	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			if sensitiveKeys[strings.ToLower(k)] {
				out[k] = Placeholder
				continue
			}
			redacted, err := redact(child, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out[k] = redacted
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			redacted, err := redact(e, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			out[i] = redacted
		}
		return out, nil
	default:
		return v, nil
	}
}
```

### Tests

The first test redacts a config with a password nested under `database` and a
secret nested inside an array of token maps, comparing the whole result with
`reflect.DeepEqual`. The depth guard test builds a chain of nested maps with
`nestedMap`, exactly at the cap and one past it. A last test confirms a bare
scalar passes through untouched.

Create `redact_test.go`:

```go
package redact

import (
	"errors"
	"reflect"
	"testing"
)

func TestRedactReplacesSensitiveKeysAtAnyDepth(t *testing.T) {
	input := map[string]any{
		"name": "svc-billing",
		"database": map[string]any{
			"host":     "db.internal",
			"password": "hunter2",
		},
		"tokens": []any{
			map[string]any{"kind": "refresh", "secret": "abc123"},
		},
	}

	got, err := Redact(input, 5)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}

	want := map[string]any{
		"name": "svc-billing",
		"database": map[string]any{
			"host":     "db.internal",
			"password": Placeholder,
		},
		"tokens": []any{
			map[string]any{"kind": "refresh", "secret": Placeholder},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Redact() = %#v, want %#v", got, want)
	}
}

func nestedMap(depth int) any {
	var v any = "leaf"
	for i := 0; i < depth; i++ {
		v = map[string]any{"child": v}
	}
	return v
}

func TestRedactDepthGuard(t *testing.T) {
	tests := []struct {
		name     string
		depth    int
		maxDepth int
		wantErr  error
	}{
		{"exactly at cap", 3, 3, nil},
		{"one past cap", 4, 3, ErrMaxDepthExceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Redact(nestedMap(tc.depth), tc.maxDepth)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Redact() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Redact() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestRedactScalarPassesThrough(t *testing.T) {
	got, err := Redact(42, 5)
	if err != nil {
		t.Fatalf("Redact() error = %v", err)
	}
	if got != 42 {
		t.Errorf("Redact(42) = %v, want 42", got)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

Redaction and depth-checking share one recursion: each call either matches a
sensitive key and substitutes the placeholder, or recurses one level deeper
and rebuilds the container from the result. `reflect.DeepEqual` on the whole
returned structure is what makes the multi-depth case checkable in one
assertion instead of walking the result by hand. The depth guard is the same
mechanical pattern as everywhere else in this lesson — check before
recursing, return a sentinel, never after the fact — applied to data that is
already in memory rather than being streamed in.

## Resources

- [encoding/json: Unmarshaling into an interface value](https://pkg.go.dev/encoding/json#Unmarshal)
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-category-tree-breadcrumb-flatten.md](10-category-tree-breadcrumb-flatten.md) | Next: [12-storage-prefix-tree-size-rollup.md](12-storage-prefix-tree-size-rollup.md)
