# Exercise 8: Reproducible PBT Runs and Bridging rapid to the Go Fuzzer

A property is only worth writing if you can replay its failures and scale its
effort. This exercise takes one property — a JSON canonicalizer that must be
idempotent and value-preserving — and exercises it three ways from a single
property function: as a `rapid` check with a pinnable seed for reproducibility, as
a native Go fuzz target via `rapid.MakeFuzz` so the coverage-guided engine drives
the same generators, and as a `MakeCheck`-adapted subtest. Along the way it lays
out the CI economics: a bounded per-PR check, a nightly high-check run, a
time-boxed fuzz job, and pinned-seed regressions.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
jsoncanon/                  independent module: example.com/jsoncanon
  go.mod                    go 1.26, requires pgregory.net/rapid
  jsoncanon.go              Canonicalize([]byte) ([]byte, error) via decode+re-marshal
  cmd/
    demo/
      main.go               runnable demo: canonicalize a scrambled object
  jsoncanon_test.go         rapid.Check + MakeCheck subtest + MakeFuzz fuzz target
```

Files: `jsoncanon.go`, `cmd/demo/main.go`, `jsoncanon_test.go`.
Implement: `Canonicalize`, which decodes JSON into a generic value and re-marshals it, yielding sorted object keys and a normalized form.
Test: one property (idempotent AND value-preserving) wired three ways — `rapid.Check`, `rapid.MakeCheck` under `t.Run`, and `rapid.MakeFuzz` under `f.Fuzz` with a seed corpus.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/24-property-based-testing/08-reproducibility-and-fuzz-bridge/cmd/demo
cd go-solutions/12-testing-ecosystem/24-property-based-testing/08-reproducibility-and-fuzz-bridge
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### One property, three drivers

`Canonicalize` decodes the input JSON into an `any` and marshals it back.
`encoding/json` marshals map keys in sorted order and normalizes number and
whitespace form, so the output is a canonical representation: two JSON documents
that differ only in key order or spacing canonicalize to the same bytes. That is
what makes a canonicalizer useful — for cache keys, for signature payloads, for
deduplication.

The property has two halves. Idempotence: `Canonicalize(Canonicalize(x))` equals
`Canonicalize(x)` — the canonical form is a fixed point. Value preservation:
decoding the canonical form yields the same logical value as decoding the original,
so canonicalization never corrupts data, only its representation. The property draws
a JSON-shaped value (via a depth-bounded recursive generator of nulls, bools,
finite numbers, strings, arrays, and objects), marshals it to get an input, and
checks both halves.

The same property function is driven three ways, and this is the exercise's point:

`rapid.Check(t, prop)` is the per-PR check. When it fails, rapid prints a seed; you
rerun with `go test -run TestCanonicalize -rapid.seed=N` to replay the exact failing
generation, and `-rapid.checks=N` to scale how many cases it tries (a small number
per PR, a large number nightly).

`rapid.MakeCheck(prop)` adapts the property into a `func(*testing.T)` you can pass to
`t.Run`, so the property composes with ordinary subtests and table drivers.

`rapid.MakeFuzz(prop)` adapts the property into a `func(*testing.T, []byte)` for
`f.Fuzz`. The native fuzzer's coverage-guided byte mutations become the entropy
that drives rapid's typed generators: the engine mutates raw bytes, rapid interprets
them as generator draws, and coverage feedback steers toward new code paths. Under a
plain `go test` the fuzz target simply runs its seed corpus as regression cases;
under `go test -fuzz=FuzzCanonicalize -fuzztime=30s` it explores. Contrast the two
minimizers: rapid shrinks *typed* values (a smaller slice, a shorter string), while
the fuzzer minimizes at the *byte* level — complementary views of "the smallest
input that fails."

Create `jsoncanon.go`:

```go
package jsoncanon

import (
	"bytes"
	"encoding/json"
)

// Canonicalize decodes JSON into a generic value and re-marshals it, producing a
// canonical form: object keys sorted, numbers and whitespace normalized. Two inputs
// that differ only in key order or spacing canonicalize to identical bytes.
func Canonicalize(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
```

### The runnable demo

The demo canonicalizes an object whose keys are out of order and whose spacing is
irregular, showing the sorted, normalized result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/jsoncanon"
)

func main() {
	raw := []byte(`{ "b": 2,  "a": 1, "c": [3, 2, 1] }`)
	out, err := jsoncanon.Canonicalize(raw)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"a":1,"b":2,"c":[3,2,1]}
```

Object keys are sorted (`a`, `b`, `c`) and whitespace is normalized, while the
array `[3,2,1]` keeps its order — arrays are ordered, objects are not.

### The property tests

The generator builds JSON-able values with a depth bound so datasets stay small and
shrinkable, using finite `Float64Range` numbers (JSON cannot represent NaN or Inf)
and string keys for objects. The single `prop` function is shared by all three
drivers. The fuzz target seeds its corpus with a scrambled object and an array so
that even a plain `go test` run exercises the property on real bytes.

Create `jsoncanon_test.go`:

```go
package jsoncanon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// jsonGen builds a JSON-representable value up to the given nesting depth.
func jsonGen(depth int) *rapid.Generator[any] {
	leaves := []*rapid.Generator[any]{
		rapid.Just[any](nil),
		rapid.Bool().AsAny(),
		rapid.Float64Range(-1e6, 1e6).AsAny(),
		rapid.StringN(0, 8, -1).AsAny(),
	}
	if depth <= 0 {
		return rapid.OneOf(leaves...)
	}
	arr := rapid.SliceOfN(jsonGen(depth-1), 0, 4).AsAny()
	obj := rapid.MapOfN(rapid.StringN(1, 6, -1), jsonGen(depth-1), 0, 4).AsAny()
	return rapid.OneOf(append(leaves, arr, obj)...)
}

// prop is the property: Canonicalize is idempotent AND preserves the decoded value.
// It is shared verbatim by rapid.Check, rapid.MakeCheck, and rapid.MakeFuzz.
func prop(t *rapid.T) {
	v := jsonGen(3).Draw(t, "value")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal seed value: %v", err)
	}

	c1, err := Canonicalize(raw)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	c2, err := Canonicalize(c1)
	if err != nil {
		t.Fatalf("Canonicalize twice: %v", err)
	}
	if !bytes.Equal(c1, c2) {
		t.Fatalf("not idempotent:\n once  %s\n twice %s", c1, c2)
	}

	var a, b any
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(c1, &b); err != nil {
		t.Fatalf("unmarshal canonical: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("value changed:\n original  %#v\n canonical %#v", a, b)
	}
}

// TestCanonicalize is the per-PR check. Replay a failure with
// -rapid.seed=N, scale coverage with -rapid.checks=N.
func TestCanonicalize(t *testing.T) {
	t.Parallel()
	rapid.Check(t, prop)
}

// TestCanonicalizeSubtest shows MakeCheck adapting the property into a subtest.
func TestCanonicalizeSubtest(t *testing.T) {
	t.Parallel()
	t.Run("idempotent-and-value-preserving", rapid.MakeCheck(prop))
}

// FuzzCanonicalize bridges the property to the native fuzzer: the coverage-guided
// engine mutates raw bytes that MakeFuzz feeds into rapid's generators. Under plain
// `go test` it runs the seed corpus; under `go test -fuzz` it explores.
func FuzzCanonicalize(f *testing.F) {
	f.Add([]byte(`{"b":1,"a":2}`))
	f.Add([]byte(`[1,2,3]`))
	f.Fuzz(rapid.MakeFuzz(prop))
}

func ExampleCanonicalize() {
	out, _ := Canonicalize([]byte(`{"b":2,"a":1}`))
	fmt.Println(string(out))
	// Output: {"a":1,"b":2}
}
```

### CI economics

The four jobs, from cheapest to most thorough. The per-PR gate runs
`go test ./...`, which executes `TestCanonicalize` at the default `-rapid.checks`
(fast, seconds) plus the fuzz seed corpus as regression cases — merge-blocking and
bounded. A nightly job runs `go test -rapid.checks=100000` (and `-rapid.steps=N` for
state machines) to explore far more of the input space without blocking merges. A
separate scheduled fuzz job runs `go test -fuzz=FuzzCanonicalize -fuzztime=10m`,
which never returns on its own and so must never be on the merge path. And for every
counterexample any of these ever finds, you commit the reproducer — a pinned
`-rapid.seed` regression or, for the fuzzer, the minimized input the engine writes
into `testdata/fuzz/` — so the bug cannot silently return.

## Review

The canonicalizer is correct when it is idempotent (the output is a fixed point) and
value-preserving (decoding the canonical form yields the original value), and the
exercise's real lesson is that one property function serves the check, the subtest,
and the fuzz target unchanged. Reproducibility is the payoff: a printed seed replays
an exact failure, `-rapid.checks` scales effort, and `MakeFuzz` lets the
coverage-guided engine drive the same generators the check uses.

The mistakes to avoid are operational. First, never put an unbounded fuzz run on the
per-PR gate: `go test -fuzz` without `-fuzztime` runs forever and blocks every merge
— the gate is bounded `rapid.checks` plus the seed corpus, and fuzzing is scheduled
separately. Second, keep the property deterministic: it must be a pure function of
its generated input, because the fuzzer and rapid both need to replay it byte-for-byte
— a `time.Now` or a map range inside `prop` makes every failure an unreproducible
flake. Third, actually commit the reproducers; a property that found a bug once
proves nothing about tomorrow unless the seed or the minimized corpus entry is checked
in. Run `go test -race`; `Canonicalize` is pure and allocates fresh buffers per call.

## Resources

- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `Check`, `MakeCheck`, `MakeFuzz`, and the `-rapid.seed`/`-rapid.checks` flags.
- [Go Fuzzing](https://go.dev/doc/security/fuzz/) — why the fuzz body must be deterministic, and how the corpus and minimization work.
- [`encoding/json`](https://pkg.go.dev/encoding/json#Marshal) — `Marshal` sorts map keys, the basis of the canonical form.

---

Back to [07-custom-generators-and-shrinking.md](07-custom-generators-and-shrinking.md) | Next: [09-roundtrip-and-bounds-id-codec.md](09-roundtrip-and-bounds-id-codec.md)
