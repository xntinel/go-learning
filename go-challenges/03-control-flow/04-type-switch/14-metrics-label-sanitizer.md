# Exercise 14: Refuse Unsafe Values in a Metrics Label Sanitizer

**Nivel: Intermedio** — validacion rapida (un test corto).

A metrics exporter attaches a string label to every series, and the label
value arrives as `any` from call sites across the codebase. Unlike a log
encoder, which renders anything it is handed, a label sanitizer must *refuse*
whole categories of input — maps, slices, structs — because stringifying them
would let one careless call site explode the series cardinality. Only
scalars, `time.Duration`, and the `error`/`fmt.Stringer` interfaces are safe.

## What you'll build

```text
labelsafe/                   independent module: example.com/labelsafe
  go.mod                     go 1.24
  labelsafe.go                Sanitize(v any) (string, error)
  labelsafe_test.go           one table test over safe and refused shapes
```

- Implement: `Sanitize(v any) (string, error)`, accepting `nil`, `string`,
  `bool`, `int`, `int64`, `float64`, `time.Duration`, `error`, and
  `fmt.Stringer`; rejecting everything else.
- Test: every accepted scalar, `time.Duration` proven distinct from a bare
  `int64`, a type implementing both `error` and `fmt.Stringer` to prove
  ordering, and a map/slice/struct each refused.

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/04-type-switch/14-metrics-label-sanitizer
cd go-solutions/03-control-flow/04-type-switch/14-metrics-label-sanitizer
go mod edit -go=1.24
```

Create `labelsafe.go`:

```go
package labelsafe

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ErrUnsafeLabel is the sentinel for a value that is refused instead of being
// stringified, because rendering it as a metrics label would risk unbounded
// cardinality.
var ErrUnsafeLabel = errors.New("unsafe metrics label value")

// Sanitize converts an arbitrary label value into the string a metrics
// exporter attaches to a series. Unlike a log encoder, which renders anything
// it is handed, a label sanitizer must refuse whole categories of input:
// maps, slices, and structs vary too widely to bound the number of resulting
// series, so they are rejected rather than stringified. Only scalars, a fixed
// duration type, and the error/Stringer interfaces are accepted.
func Sanitize(v any) (string, error) {
	switch s := v.(type) {
	case nil:
		return "", nil
	case string:
		return s, nil
	case bool:
		return strconv.FormatBool(s), nil
	case int:
		return strconv.Itoa(s), nil
	case int64:
		return strconv.FormatInt(s, 10), nil
	case float64:
		return strconv.FormatFloat(s, 'g', -1, 64), nil
	case time.Duration:
		return s.String(), nil
	case error:
		return s.Error(), nil
	case fmt.Stringer:
		return s.String(), nil
	default:
		return "", fmt.Errorf("%w: refusing to render %T as a label", ErrUnsafeLabel, v)
	}
}
```

Create `labelsafe_test.go`:

```go
package labelsafe

import (
	"errors"
	"testing"
	"time"
)

// multiKind implements both error and fmt.Stringer, to prove the error case
// (listed first) wins over the Stringer case for a value satisfying both.
type multiKind struct{}

func (multiKind) Error() string  { return "error-form" }
func (multiKind) String() string { return "string-form" }

type region struct{ name string }

func (r region) String() string { return r.name }

func TestSanitize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{"nil is empty", nil, "", false},
		{"string passthrough", "checkout", "checkout", false},
		{"bool true", true, "true", false},
		{"int", 42, "42", false},
		{"int64", int64(42), "42", false},
		{"float64", 3.5, "3.5", false},
		{"duration is distinct from int64", 250 * time.Millisecond, "250ms", false},
		{"error wins over stringer", multiKind{}, "error-form", false},
		{"stringer only", region{name: "us-east"}, "us-east", false},
		{"bare error", errors.New("boom"), "boom", false},
		{"map rejected for cardinality", map[string]string{"a": "b"}, "", true},
		{"slice rejected for cardinality", []string{"a", "b"}, "", true},
		{"struct without Stringer rejected", struct{ X int }{X: 1}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Sanitize(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrUnsafeLabel) {
					t.Fatalf("Sanitize(%v) err = %v, want ErrUnsafeLabel", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sanitize(%v) unexpected error: %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("Sanitize(%v) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`time.Duration` needs its own case even though its underlying type is
`int64`, because a type switch matches the dynamic type exactly, not the
underlying type — without that case a duration would fall to `default` and be
refused. `case error` is listed before `case fmt.Stringer` deliberately: the
test's `multiKind` type implements both, and the first matching case wins, so
error formatting takes priority over `String()` for any type that offers
both. The `default` branch is the actual point of the exercise: it protects
the exporter by refusing maps, slices, and structs instead of stringifying
them into an unbounded number of series.

## Resources

- [Prometheus: instrumenting, label cardinality](https://prometheus.io/docs/practices/naming/#labels)
- [fmt.Stringer](https://pkg.go.dev/fmt#Stringer)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-feature-flag-value-normalizer.md](13-feature-flag-value-normalizer.md) | Next: [15-idempotency-key-validator.md](15-idempotency-key-validator.md)
