# Exercise 14: Fallback — Try a Primary Lookup, Fall Back to a Secondary

**Nivel: Intermedio** — validacion rapida (un test corto).

A pricing service that is down should not take checkout down with it if a
secondary source can answer instead. `Fallback` composes two lookups of
the same shape into one: try the primary, and only call the secondary if
the primary failed — never both, never neither.

## What you'll build

```text
fallback/                   independent module: example.com/fallback
  go.mod                    go 1.24
  fallback.go               type Lookup[K, V]; func Fallback
  fallback_test.go          primary-succeeds, primary-fails, both-fail cases
```

- Files: `fallback.go`, `fallback_test.go`.
- Implement: `Lookup[K comparable, V any] func(key K) (V, error)` and `Fallback[K, V](primary, secondary Lookup[K, V]) Lookup[K, V]`.
- Test: primary success returns primary's value and never calls secondary; primary failure calls secondary and returns its value on success; both failing returns a joined error recoverable via `errors.Is` for either cause.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/14-fallback-lookup-combinator
cd go-solutions/04-functions/10-higher-order-functions/14-fallback-lookup-combinator
go mod edit -go=1.24
```

### Try once, fall back once, join on total failure

`Fallback` is a factory that closes over two `Lookup` values and returns a
third with the same signature — so a `Fallback`-wrapped lookup is itself a
valid `primary` or `secondary` argument to another `Fallback` call, letting
a caller chain a third source if the product needs it. The body has three
outcomes: primary succeeds and secondary is never touched; primary fails
and secondary's result (success or failure) decides the outcome; both fail
and the two errors are joined with `errors.Join` so a caller can recover
either cause with `errors.Is`, rather than losing the primary's failure
reason the moment the secondary is consulted.

Create `fallback.go`:

```go
package fallback

import "errors"

// Lookup fetches a value for key from some dependency — a pricing service,
// a feature-flag source, a config store — that can fail.
type Lookup[K comparable, V any] func(key K) (V, error)

// Fallback composes primary and secondary into one Lookup: it tries
// primary first, and only calls secondary if primary returns a non-nil
// error. secondary never runs when primary succeeds. If both fail, the
// returned error joins both causes so a caller can inspect either with
// errors.Is/As.
func Fallback[K comparable, V any](primary, secondary Lookup[K, V]) Lookup[K, V] {
	return func(key K) (V, error) {
		v, err := primary(key)
		if err == nil {
			return v, nil
		}
		v, secErr := secondary(key)
		if secErr == nil {
			return v, nil
		}
		var zero V
		return zero, errors.Join(err, secErr)
	}
}
```

### Tests

The table covers the three outcomes a fallback can produce. A
`secondaryCalled` flag proves the "never touch secondary on primary
success" half of the contract as directly as the return value proves the
other half; the both-fail case asserts both original errors survive inside
the joined result.

Create `fallback_test.go`:

```go
package fallback

import (
	"errors"
	"testing"
)

var (
	errPrimaryDown   = errors.New("primary pricing service unavailable")
	errSecondaryDown = errors.New("secondary pricing cache miss")
)

func TestFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		primaryErr    error
		secondaryErr  error
		wantValue     int
		wantSecondary bool // whether secondary must have been called
		wantErr       bool
	}{
		{
			name:          "primary succeeds, secondary never runs",
			primaryErr:    nil,
			wantValue:     100,
			wantSecondary: false,
		},
		{
			name:          "primary fails, secondary succeeds",
			primaryErr:    errPrimaryDown,
			secondaryErr:  nil,
			wantValue:     200,
			wantSecondary: true,
		},
		{
			name:          "both fail, error joins both causes",
			primaryErr:    errPrimaryDown,
			secondaryErr:  errSecondaryDown,
			wantSecondary: true,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var secondaryCalled bool
			primary := func(key string) (int, error) {
				if tc.primaryErr != nil {
					return 0, tc.primaryErr
				}
				return 100, nil
			}
			secondary := func(key string) (int, error) {
				secondaryCalled = true
				if tc.secondaryErr != nil {
					return 0, tc.secondaryErr
				}
				return 200, nil
			}

			f := Fallback(primary, secondary)
			got, err := f("sku-1")

			if secondaryCalled != tc.wantSecondary {
				t.Errorf("secondary called = %v, want %v", secondaryCalled, tc.wantSecondary)
			}

			if tc.wantErr {
				if err == nil {
					t.Fatal("f() err = nil, want a joined error")
				}
				if !errors.Is(err, errPrimaryDown) {
					t.Error("joined error does not wrap errPrimaryDown")
				}
				if !errors.Is(err, errSecondaryDown) {
					t.Error("joined error does not wrap errSecondaryDown")
				}
				return
			}

			if err != nil {
				t.Fatalf("f() unexpected err: %v", err)
			}
			if got != tc.wantValue {
				t.Errorf("f() = %d, want %d", got, tc.wantValue)
			}
		})
	}
}
```

## Review

`Fallback` is correct when secondary is called in exactly one branch —
primary's failure — and never in the success branch; the `secondaryCalled`
assertion in the test is what actually pins that down, since the return
value alone cannot distinguish "secondary ran and happened to agree" from
"secondary never ran." Joining both errors on total failure, instead of
returning only the secondary's, keeps the primary's failure reason
recoverable — useful when an on-call engineer needs to know the primary
service is down even though the secondary also failed to compensate. The
generic `Lookup[K, V]` signature means the same `Fallback` composes over a
pricing lookup, a feature-flag lookup, or any other keyed, fallible read.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple failures into one inspectable error.
- [Google SRE Workbook: Cascading Failures](https://sre.google/sre-book/addressing-cascading-failures/) — why a fallback path matters when a dependency degrades.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-instrumentation-decorator.md](13-instrumentation-decorator.md) | Next: [15-sliding-window-throttle-with-history.md](15-sliding-window-throttle-with-history.md)
