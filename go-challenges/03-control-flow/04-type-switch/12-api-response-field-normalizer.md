# Exercise 12: Normalize an Optional Field Across API Encoding Quirks

**Nivel: Intermedio** — validacion rapida (un test corto).

A third-party API's "count" field is encoded inconsistently across versions
and endpoints: a JSON number when present, an empty string or JSON `null`
when absent, and — because one endpoint has a known bug — a bare JSON
`false` standing in for "not applicable" instead of proper `null`. The
normalizer must tell "no value" apart from "value zero" using only the
decoded value's shape.

## What you'll build

```text
apifields/                  independent module: example.com/apifields
  go.mod                     go 1.24
  apifields.go               NormalizeOptionalCount(v any) (*int64, error)
  apifields_test.go          one table test over every absent/present shape
```

- Implement: `NormalizeOptionalCount(v any) (*int64, error)`, returning `nil`
  for absent (`nil`, `false`, empty string) and a pointer for present
  (`json.Number`, numeric string, integral `float64`).
- Test: every absent encoding, a valid numeric string and `json.Number`, a
  garbage string, an integral and a fractional `float64`, and the buggy
  bare-`true` case.

Set up the module:

```bash
mkdir -p ~/go-exercises/apifields
cd ~/go-exercises/apifields
go mod init example.com/apifields
go mod edit -go=1.24
```

Create `apifields.go`:

```go
package apifields

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ErrInvalidCount is the sentinel for a "count" field that cannot be
// normalized.
var ErrInvalidCount = errors.New("invalid count field")

// NormalizeOptionalCount normalizes a third-party API's "count" field into an
// optional int64. Across API versions and endpoints the same logical field is
// encoded inconsistently: a JSON number when present, an empty string or JSON
// null when absent, and — because one upstream endpoint has a known bug — a
// bare JSON false standing in for "not applicable" instead of proper null. A
// nil return means the field is absent; the switch must tell "no value" apart
// from "value zero" using the value's own decoded shape.
func NormalizeOptionalCount(v any) (*int64, error) {
	switch c := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if c {
			return nil, fmt.Errorf("%w: bare true is not a valid count", ErrInvalidCount)
		}
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidCount, c, err)
		}
		return &n, nil
	case json.Number:
		n, err := c.Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidCount, c.String(), err)
		}
		return &n, nil
	case float64:
		if c != math.Trunc(c) {
			return nil, fmt.Errorf("%w: %v has a fractional part", ErrInvalidCount, c)
		}
		n := int64(c)
		return &n, nil
	default:
		return nil, fmt.Errorf("%w: cannot normalize %T", ErrInvalidCount, v)
	}
}
```

Create `apifields_test.go`:

```go
package apifields

import (
	"encoding/json"
	"errors"
	"testing"
)

func int64p(n int64) *int64 { return &n }

func TestNormalizeOptionalCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   any
		want    *int64
		wantErr bool
	}{
		{"absent null", nil, nil, false},
		{"absent bool false", false, nil, false},
		{"invalid bool true", true, nil, true},
		{"empty string is absent", "", nil, false},
		{"numeric string", "42", int64p(42), false},
		{"garbage string", "n/a", nil, true},
		{"json.Number", json.Number("7"), int64p(7), false},
		{"integral float64", 42.0, int64p(42), false},
		{"fractional float64", 42.5, nil, true},
		{"unsupported type", []int{1}, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeOptionalCount(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidCount) {
					t.Fatalf("NormalizeOptionalCount(%v) err = %v, want ErrInvalidCount", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeOptionalCount(%v) unexpected error: %v", tt.value, err)
			}
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("NormalizeOptionalCount(%v) = %d, want nil", tt.value, *got)
			case tt.want != nil && got == nil:
				t.Fatalf("NormalizeOptionalCount(%v) = nil, want %d", tt.value, *tt.want)
			case tt.want != nil && got != nil && *got != *tt.want:
				t.Fatalf("NormalizeOptionalCount(%v) = %d, want %d", tt.value, *got, *tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

`case bool` is the load-bearing branch here: it exists purely to absorb a
known upstream encoding bug, and it rejects a bare `true` rather than
guessing what it should mean. Note the ordering discipline still applies even
without interface cases — `string` and `json.Number` are checked for a valid
numeric form rather than trusting the source, and `float64` is guarded with
`math.Trunc` exactly as in a pure numeric normalizer, because a fractional
"count" is a data-quality bug worth surfacing, not silently truncating.

## Resources

- [encoding/json.Number](https://pkg.go.dev/encoding/json#Number)
- [strconv.ParseInt](https://pkg.go.dev/strconv#ParseInt)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-cache-value-decoder.md](11-cache-value-decoder.md) | Next: [13-feature-flag-value-normalizer.md](13-feature-flag-value-normalizer.md)
