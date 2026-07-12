# Exercise 13: Classify a Feature Flag Value by Its Rollout Strategy

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature-flag SDK returns an evaluated flag's value as `any`, and the Go type
of that value *is* the rollout strategy: a `bool` is a plain toggle, a
`string` names a variant, a `float64` in `[0, 100]` is a percentage-rollout
threshold, and a `[]any` is a multivariate list of allowed options. The
evaluator must classify each shape into one normalized `Flag`.

## What you'll build

```text
flagvalue/                  independent module: example.com/flagvalue
  go.mod                     go 1.24
  flagvalue.go               Normalize(v any) (Flag, error)
  flagvalue_test.go          one table test over every rollout strategy
```

- Implement: `Normalize(v any) (Flag, error)`, classifying `bool`, `string`,
  `float64`, and `[]any` into a `Flag{Kind, Variant, Percent, Options}`.
- Test: on/off toggle, named variant plus the empty-variant error, an
  in-range and an out-of-range percentage, a valid multivariate list plus one
  with a non-string option, a missing flag, and an unsupported type.

Set up the module:

```bash
go mod edit -go=1.24
```

Create `flagvalue.go`:

```go
package flagvalue

import (
	"errors"
	"fmt"
)

// ErrInvalidFlag is the sentinel for a flag value the evaluator cannot
// classify.
var ErrInvalidFlag = errors.New("invalid flag value")

// Kind is the evaluated shape of a feature flag value returned by the flag
// SDK, which encodes different rollout strategies as different Go types.
type Kind int

const (
	KindOff Kind = iota
	KindOn
	KindVariant
	KindPercentage
	KindMultivariate
)

// Flag is the normalized decision derived from a raw SDK value.
type Flag struct {
	Kind    Kind
	Variant string  // set for KindVariant
	Percent float64 // set for KindPercentage, 0..100
	Options []string
}

// Normalize classifies a raw feature-flag value from the evaluation SDK. A
// bool is a simple on/off toggle. A string names the selected variant. A
// float64 in [0, 100] is a percentage-rollout threshold. A []any is a
// multivariate list of allowed option names. A nil value means the flag is
// unknown to the SDK and the caller must fall back to its own default.
func Normalize(v any) (Flag, error) {
	switch f := v.(type) {
	case nil:
		return Flag{}, fmt.Errorf("%w: flag not found", ErrInvalidFlag)
	case bool:
		if f {
			return Flag{Kind: KindOn}, nil
		}
		return Flag{Kind: KindOff}, nil
	case string:
		if f == "" {
			return Flag{}, fmt.Errorf("%w: empty variant name", ErrInvalidFlag)
		}
		return Flag{Kind: KindVariant, Variant: f}, nil
	case float64:
		if f < 0 || f > 100 {
			return Flag{}, fmt.Errorf("%w: percentage %v out of [0,100]", ErrInvalidFlag, f)
		}
		return Flag{Kind: KindPercentage, Percent: f}, nil
	case []any:
		opts := make([]string, 0, len(f))
		for _, item := range f {
			s, ok := item.(string)
			if !ok {
				return Flag{}, fmt.Errorf("%w: multivariate option %T is not a string", ErrInvalidFlag, item)
			}
			opts = append(opts, s)
		}
		return Flag{Kind: KindMultivariate, Options: opts}, nil
	default:
		return Flag{}, fmt.Errorf("%w: cannot normalize %T", ErrInvalidFlag, v)
	}
}
```

Create `flagvalue_test.go`:

```go
package flagvalue

import (
	"errors"
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   any
		want    Flag
		wantErr bool
	}{
		{"on", true, Flag{Kind: KindOn}, false},
		{"off", false, Flag{Kind: KindOff}, false},
		{"variant", "dark-mode-v2", Flag{Kind: KindVariant, Variant: "dark-mode-v2"}, false},
		{"empty variant", "", Flag{}, true},
		{"percentage", 25.5, Flag{Kind: KindPercentage, Percent: 25.5}, false},
		{"percentage out of range", 150.0, Flag{}, true},
		{"multivariate", []any{"a", "b", "c"}, Flag{Kind: KindMultivariate, Options: []string{"a", "b", "c"}}, false},
		{"multivariate bad option", []any{"a", 2}, Flag{}, true},
		{"not found", nil, Flag{}, true},
		{"unsupported type", 7, Flag{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tt.value)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidFlag) {
					t.Fatalf("Normalize(%v) err = %v, want ErrInvalidFlag", tt.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%v) unexpected error: %v", tt.value, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Normalize(%v) = %+v, want %+v", tt.value, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Each concrete case owns one rollout strategy, and none of them overlaps in
type, so ordering does not matter here the way it does with interface cases —
what matters instead is that every branch validates its own shape before
returning `KindX`: an empty variant name, an out-of-range percentage, and a
non-string multivariate option are all rejected rather than accepted as
degenerate values. `case nil` treats "flag not found" as a caller-visible
error instead of silently defaulting to off, which is the one behavior a flag
evaluator must never get wrong.

## Resources

- [A Tour of Go: Type switches](https://go.dev/tour/methods/16)
- [Go Blog: JSON and Go (array/object decoding into any)](https://go.dev/blog/json)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-api-response-field-normalizer.md](12-api-response-field-normalizer.md) | Next: [14-metrics-label-sanitizer.md](14-metrics-label-sanitizer.md)
