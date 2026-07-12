# Exercise 1: Typed Query Accessors Returning (value, error)

Every HTTP handler eventually reaches for a query parameter and has to turn a
string into a typed value that might be malformed. This exercise builds the
canonical `(value, error)` accessors — `Int`, `Int64`, `Bool`, `Duration` — over
`net/url.Values`, each wrapping the underlying `strconv`/`time` failure with `%w`
so the caller can classify it with `errors.Is`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
accessors/                 independent module: example.com/accessors
  go.mod                   go 1.25
  accessors.go             type Query embeds url.Values; Int/Int64/Bool/Duration -> (T, error)
  cmd/
    demo/
      main.go              parses a real query string, prints typed values and a wrapped error
  accessors_test.go        table tests; errors.Is against strconv.ErrSyntax/ErrRange; -race
```

- Files: `accessors.go`, `cmd/demo/main.go`, `accessors_test.go`.
- Implement: `Query` embedding `url.Values`; free functions `Int`, `Int64`, `Bool`, `Duration` each returning `(T, error)`, wrapping the stdlib failure with `%w` and returning a "missing key" error when the key is absent.
- Test: valid input parses; invalid syntax returns an error matched by `errors.Is(err, strconv.ErrSyntax)`; `Int64` overflow matches `strconv.ErrRange`; a missing key returns non-nil; an invalid `Duration` asserts on the message.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why (value, error) is the shape here

Parsing `?page=abc` can fail, and the failure is *informative*: the caller wants
to reply `400` with a message that says which key was bad and why. A bare
`(int, bool)` would collapse "the key was absent", "the value was not a number",
and "the number overflowed" into one indistinguishable `false`. The
`(value, error)` shape keeps those apart, and wrapping the stdlib error with `%w`
lets the caller go further and ask *which* failure it was:
`errors.Is(err, strconv.ErrSyntax)` for malformed input,
`errors.Is(err, strconv.ErrRange)` for overflow.

The accessors are free functions taking a `Query` rather than methods so that the
type parameter of the result lives in the function name (`Int`, `Int64`), which
reads naturally at the call site: `page, err := Int(q, "page")`. Each first pulls
the raw string through `First` (the `(value, ok)` shape), returns a "missing key"
error if absent, then converts and wraps.

One honest limitation shows up with `Duration`: `time.ParseDuration` returns an
*unexported* error type and exports no sentinel, so `errors.Is`/`errors.As` cannot
match it. For that one accessor the test asserts on the message
(`"invalid duration"`) instead. Knowing which stdlib errors expose a sentinel and
which do not is part of using this shape well.

Create `accessors.go`:

```go
package qparser

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Query is a thin wrapper over url.Values. Embedding promotes Get, Has, Add,
// Set, Del, and Encode for free; the typed accessors below add the parsing the
// stdlib does not.
type Query struct {
	url.Values
}

// Parse wraps already-parsed url.Values (for example from r.URL.Query()).
func Parse(v url.Values) Query {
	return Query{Values: v}
}

// First returns the first value for key and whether the key was present. This
// is the (value, ok) shape: absence is a normal outcome, not an error.
func (q Query) First(key string) (string, bool) {
	values := q.Values[key]
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// Int parses the first value for key as a base-10 int.
func Int(q Query, key string) (int, error) {
	raw, ok := q.First(key)
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %q as int: %w", key, err)
	}
	return v, nil
}

// Int64 parses the first value for key as a base-10 int64.
func Int64(q Query, key string) (int64, error) {
	raw, ok := q.First(key)
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as int64: %w", key, err)
	}
	return v, nil
}

// Bool parses the first value for key with strconv.ParseBool.
func Bool(q Query, key string) (bool, error) {
	raw, ok := q.First(key)
	if !ok {
		return false, fmt.Errorf("missing key %q", key)
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %q as bool: %w", key, err)
	}
	return v, nil
}

// Duration parses the first value for key with time.ParseDuration. time exports
// no sentinel for a bad duration, so callers assert on the message, not errors.Is.
func Duration(q Query, key string) (time.Duration, error) {
	raw, ok := q.First(key)
	if !ok {
		return 0, fmt.Errorf("missing key %q", key)
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %q as duration: %w", key, err)
	}
	return d, nil
}
```

### The runnable demo

The demo parses a realistic query string, unpacks each accessor at the call site,
and deliberately triggers the error path on a malformed `page` to show the wrapped
message.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/url"

	"example.com/accessors"
)

func main() {
	v, _ := url.ParseQuery("page=2&timeout=750ms&active=true")
	q := qparser.Parse(v)

	page, _ := qparser.Int(q, "page")
	timeout, _ := qparser.Duration(q, "timeout")
	active, _ := qparser.Bool(q, "active")
	fmt.Printf("page=%d timeout=%s active=%t\n", page, timeout, active)

	bad, _ := url.ParseQuery("page=abc")
	if _, err := qparser.Int(qparser.Parse(bad), "page"); err != nil {
		fmt.Println("error:", err)
	}
}
```

Note the import path is `example.com/accessors` but the package name is
`qparser` — the demo refers to it as `qparser` because that is the package's
declared name, not its import path.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
page=2 timeout=750ms active=true
error: parse "page" as int: strconv.Atoi: parsing "abc": invalid syntax
```

### Tests

The tests are table-driven where the shape allows and single-purpose where an
assertion needs a specific sentinel. `TestIntRejectsInvalidSyntax` is the core of
the lesson: it proves the wrap contract with `errors.Is(err, strconv.ErrSyntax)`.
`TestInt64RejectsOverflow` proves the same for `strconv.ErrRange`.
`TestDurationRejectsInvalidFormat` documents the message-only limitation.

Create `accessors_test.go`:

```go
package qparser

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) Query {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	return Parse(v)
}

func TestIntValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"positive", "page=42", 42},
		{"zero", "page=0", 0},
		{"negative", "page=-3", -3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Int(mustParse(t, tc.raw), "page")
			if err != nil {
				t.Fatalf("Int: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("Int = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestIntMissingKey(t *testing.T) {
	t.Parallel()
	if _, err := Int(mustParse(t, ""), "page"); err == nil {
		t.Fatal("Int on missing key: want error, got nil")
	}
}

func TestIntRejectsInvalidSyntax(t *testing.T) {
	t.Parallel()
	_, err := Int(mustParse(t, "page=abc"), "page")
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("err = %v, want a wrap of strconv.ErrSyntax", err)
	}
}

func TestInt64RejectsOverflow(t *testing.T) {
	t.Parallel()
	_, err := Int64(mustParse(t, "k=99999999999999999999"), "k")
	if !errors.Is(err, strconv.ErrRange) {
		t.Fatalf("err = %v, want a wrap of strconv.ErrRange", err)
	}
}

func TestBoolRejectsInvalidSyntax(t *testing.T) {
	t.Parallel()
	_, err := Bool(mustParse(t, "active=maybe"), "active")
	if !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("err = %v, want a wrap of strconv.ErrSyntax", err)
	}
}

func TestDurationValid(t *testing.T) {
	t.Parallel()
	d, err := Duration(mustParse(t, "timeout=750ms"), "timeout")
	if err != nil {
		t.Fatalf("Duration: unexpected error %v", err)
	}
	if d != 750*time.Millisecond {
		t.Fatalf("Duration = %s, want 750ms", d)
	}
}

func TestDurationRejectsInvalidFormat(t *testing.T) {
	t.Parallel()
	_, err := Duration(mustParse(t, "timeout=soon"), "timeout")
	if err == nil {
		t.Fatal("Duration on bad input: want error, got nil")
	}
	// time exports no sentinel for a bad duration, so assert on the message.
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("err = %v, want it to mention 'invalid duration'", err)
	}
}

func ExampleInt() {
	v, _ := url.ParseQuery("page=7")
	page, err := Int(Parse(v), "page")
	fmt.Println(page, err)
	// Output: 7 <nil>
}
```

The `Example` function is auto-verified by `go test`: its printed output must
match the `// Output:` comment exactly, which pins the `(value, error)` shape at
the call site — `7 <nil>` on success.

## Review

The accessors are correct when a valid string yields the typed value with a nil
error, a missing key yields a non-nil "missing key" error, and a malformed value
yields an error whose cause is still reachable with `errors.Is`. The proof is
`TestIntRejectsInvalidSyntax` and `TestInt64RejectsOverflow`: if either fails, an
accessor used `%v` instead of `%w` somewhere and severed the chain. The
`Duration` message assertion is not a weaker test — it documents a real stdlib gap
where no sentinel exists, and reaching for `errors.Is(err, strconv.ErrSyntax)`
there would be wrong because `time` never wraps that sentinel.

Do not be tempted to return `(value, bool)` from these accessors "to keep it
simple". The whole reason to parse at the edge is to reject bad input with a
reason, and a bool has no reason. Run `go test -race` to confirm the accessors are
safe under concurrent handler calls (they hold no shared state, so they are).

## Resources

- [net/url.Values](https://pkg.go.dev/net/url#Values) — the map type the accessors read and the methods embedding promotes.
- [strconv package](https://pkg.go.dev/strconv) — `Atoi`, `ParseInt`, `ParseBool`, and the `ErrSyntax`/`ErrRange` sentinels.
- [errors.Is](https://pkg.go.dev/errors#Is) — how a wrapped `%w` chain is matched against a sentinel.
- [time.ParseDuration](https://pkg.go.dev/time#ParseDuration) — the one accessor whose error exports no sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-value-ok-lookup-and-embedding.md](02-value-ok-lookup-and-embedding.md)
