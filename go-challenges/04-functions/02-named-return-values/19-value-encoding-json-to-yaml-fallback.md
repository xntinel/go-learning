# Exercise 19: Encode Value with JSON-to-YAML Fallback Strategy

`encoding/json` refuses to marshal a handful of values outright — `NaN` and
`+/-Inf` floats chief among them, per its own documentation — which means a
caller can hand `Encode` a perfectly reasonable-looking map and get a hard
failure back. This exercise builds an encoder that tries JSON first and
falls back to a minimal flat encoding when JSON cannot represent the value,
using a deferred closure that rewrites the named string result instead of
inline branching at the call site.

**Nivel: Intermedio** — validacion rapida (un test corto por caso).

## What you'll build

```text
flexenc/                    independent module: example.com/flexenc
  go.mod
  flexenc.go                 Encode (named out, deferred fallback); encodeFlat helper
  cmd/demo/
    main.go                  runnable demo: clean values vs a NaN that forces fallback
  flexenc_test.go             JSON success path, NaN fallback, Inf fallback
```

- Files: `flexenc.go`, `cmd/demo/main.go`, `flexenc_test.go`.
- Implement: `Encode(v map[string]float64) (out string, err error)` that tries `json.Marshal` and, on failure, falls back to a sorted flat `"key: value"` encoding via a deferred closure.
- Test: a clean map encodes as JSON; a map containing `NaN` falls back and returns `err == nil`; a map containing `+Inf` falls back too.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/flexenc/cmd/demo
cd ~/go-exercises/flexenc
go mod init example.com/flexenc
go mod edit -go=1.24
```

### A defer that changes the outcome, not just cleans up

Every other exercise in this file uses a defer to release something or log
something after the fact. This one is different: the defer actually *rewrites
the named result* to change what the caller receives.

```go
defer func() {
    if err != nil {
        out = encodeFlat(v)
        err = nil
    }
}()

b, jerr := json.Marshal(v)
if jerr != nil {
    err = jerr
    return
}
out = string(b)
return
```

The main body only ever sets `err` when JSON marshaling fails; it never
touches the fallback logic. The deferred closure inspects `err` *after* the
body has run and, if it is non-nil, replaces `out` with the fallback encoding
and clears `err` to nil — so from the caller's point of view, `Encode` almost
never fails; it degrades. This is the named-return idiom worth taking away
from this exercise: a defer is not only for cleanup, it can also be the one
place that implements a fallback strategy, keeping that strategy out of the
main control flow entirely.

Create `flexenc.go`:

```go
package flexenc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Encode renders v as JSON. encoding/json rejects a handful of values it
// cannot represent — most notably NaN and +/-Inf floats, per the encoding/json
// docs — and in that case Encode falls back to a minimal flat "key: value"
// encoding instead of failing the caller outright.
//
// out is a named result specifically so the fallback can live in a deferred
// closure: the closure inspects the named err *after* json.Marshal has run,
// and if it is non-nil, overwrites out with the fallback rendering and clears
// err. That is the named-return idiom this exercise is about — a defer that
// changes the reported outcome, not just a defer that cleans something up.
func Encode(v map[string]float64) (out string, err error) {
	defer func() {
		if err != nil {
			out = encodeFlat(v)
			err = nil
		}
	}()

	b, jerr := json.Marshal(v)
	if jerr != nil {
		err = jerr
		return
	}
	out = string(b)
	return
}

// encodeFlat renders v as sorted "key: value" lines. It is intentionally not
// a general YAML encoder — only a flat map of float64 scalars — but it can
// represent NaN and Inf, which is exactly the case JSON cannot.
func encodeFlat(v map[string]float64) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %v", k, v[k])
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"

	"example.com/flexenc"
)

func main() {
	out, err := flexenc.Encode(map[string]float64{"a": 1, "b": 2.5})
	fmt.Printf("clean values: err=%v out=%s\n", err, out)

	out, err = flexenc.Encode(map[string]float64{"a": 1, "b": math.NaN()})
	fmt.Printf("with NaN: err=%v out=%q\n", err, out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean values: err=<nil> out={"a":1,"b":2.5}
with NaN: err=<nil> out="a: 1\nb: NaN"
```

### Tests

Create `flexenc_test.go`:

```go
package flexenc

import (
	"math"
	"testing"
)

func TestEncodeUsesJSONWhenPossible(t *testing.T) {
	t.Parallel()

	out, err := Encode(map[string]float64{"a": 1, "b": 2.5})
	if err != nil {
		t.Fatalf("Encode: unexpected error: %v", err)
	}
	want := `{"a":1,"b":2.5}`
	if out != want {
		t.Fatalf("Encode = %q, want %q", out, want)
	}
}

func TestEncodeFallsBackOnNaN(t *testing.T) {
	t.Parallel()

	out, err := Encode(map[string]float64{"a": 1, "b": math.NaN()})
	if err != nil {
		t.Fatalf("Encode: unexpected error after fallback: %v", err)
	}
	want := "a: 1\nb: NaN"
	if out != want {
		t.Fatalf("Encode = %q, want %q", out, want)
	}
}

func TestEncodeFallsBackOnInf(t *testing.T) {
	t.Parallel()

	out, err := Encode(map[string]float64{"only": math.Inf(1)})
	if err != nil {
		t.Fatalf("Encode: unexpected error after fallback: %v", err)
	}
	want := "only: +Inf"
	if out != want {
		t.Fatalf("Encode = %q, want %q", out, want)
	}
}
```

## Review

`Encode` is correct when a JSON-representable map returns compact JSON, and
anything JSON rejects returns a sorted flat rendering with a nil error
instead of propagating the marshal failure. The named result `out` is what
makes the fallback a one-line defer rather than a duplicated `if err != nil`
branch at every call site that would otherwise need to know about the
fallback format. The mistake to avoid is designing the fallback to also fail
sometimes and forgetting to reset `err` in the defer — the whole point of
this pattern is that the caller sees `err == nil` whenever *either* encoding
succeeded.

## Resources

- [`encoding/json` package docs (marshaling of floating-point values)](https://pkg.go.dev/encoding/json)
- [`math.NaN`](https://pkg.go.dev/math#NaN)
- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-db-statement-cache-eviction-guard.md](18-db-statement-cache-eviction-guard.md) | Next: [20-operation-deadline-hard-cancel.md](20-operation-deadline-hard-cancel.md)
