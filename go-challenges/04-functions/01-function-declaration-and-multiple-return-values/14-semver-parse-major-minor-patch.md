# Exercise 14: Parsing A Version String Into (major, minor, patch, err)

**Nivel: Intermedio** — validacion rapida (un test corto).

Deployment tooling, dependency resolvers, and compatibility checks all start
by pulling a version string like `v2.0.10` apart into comparable numbers.
This exercise builds `ParseVersion(s) (major int, minor int, patch int, err
error)`, a four-value return that turns a raw string into three named
integers a caller can compare directly instead of re-parsing.

This module is fully self-contained: its own `go mod init`, all code inline,
one quick test file.

## What you'll build

```text
semver/                    independent module: example.com/semver-parse
  go.mod                   go 1.24
  semver.go                package semver; ParseVersion(s) (major, minor, patch, err); ErrMalformedVersion/ErrInvalidComponent
  semver_test.go           one table test; errors.Is against both sentinels
```

- Files: `semver.go`, `semver_test.go`.
- Implement: `ParseVersion(s string) (major int, minor int, patch int, err error)` stripping an optional leading `v`, splitting on `.`, requiring exactly three parts, and converting each with `strconv.Atoi` rejecting negatives.
- Test: a table over a plain version, a `v`-prefixed version, an all-zero version, too few parts, too many parts, a non-numeric component, and a negative component, asserting `errors.Is` against `ErrMalformedVersion` or `ErrInvalidComponent`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/14-semver-parse-major-minor-patch
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/14-semver-parse-major-minor-patch
go mod edit -go=1.24
```

### Four returns, each with one job

`ParseVersion` returns four values, and the first three are the entire reason
the fourth exists: `major`, `minor`, and `patch` are each meaningless on their
own until the caller knows parsing succeeded, which is exactly what `err`
signals. This is the same "value is only trustworthy when `err == nil`"
contract from the concepts file, just wider — with three success values
instead of one, `err` still has to be checked before touching any of them,
and on failure all three are zero.

Shape validation and component validation are different failure classes with
different fixes for the caller, so they get different sentinels:
`ErrMalformedVersion` when the string does not split into exactly three
dot-separated parts (missing a patch number, or carrying pre-release suffixes
this parser deliberately does not handle); `ErrInvalidComponent` when a part
is present but is not a valid non-negative integer. A tool that wants to
report "not a version string" versus "version has a bad number" branches on
which sentinel came back.

Create `semver.go`:

```go
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedVersion is returned when the input is not exactly three
// dot-separated parts (an optional leading "v" is stripped first).
var ErrMalformedVersion = errors.New("malformed version")

// ErrInvalidComponent is returned when a part is present but is not a valid
// non-negative integer.
var ErrInvalidComponent = errors.New("invalid version component")

// ParseVersion parses a "MAJOR.MINOR.PATCH" string, with an optional leading
// "v", into its three numeric components. This deliberately does not handle
// pre-release or build-metadata suffixes ("-rc.1", "+build5") — callers that
// need full semver 2.0 compliance should trim those before calling, or reach
// for a dedicated semver package.
func ParseVersion(s string) (major int, minor int, patch int, err error) {
	trimmed := strings.TrimPrefix(s, "v")

	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("%q: %w", s, ErrMalformedVersion)
	}

	nums := make([]int, 3)
	for i, part := range parts {
		n, convErr := strconv.Atoi(part)
		if convErr != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("%q: component %q: %w", s, part, ErrInvalidComponent)
		}
		nums[i] = n
	}

	return nums[0], nums[1], nums[2], nil
}
```

At the call site: `major, minor, patch, err := semver.ParseVersion(raw)`,
handling `err` first. When only the major version matters — say, for a
breaking-change gate — the rest are discarded with the blank identifier:
`major, _, _, err := semver.ParseVersion(raw)`.

### Test

Create `semver_test.go`:

```go
package semver

import (
	"errors"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantMajor int
		wantMinor int
		wantPatch int
		wantErr   error // nil means no error expected
	}{
		{"plain", "1.4.2", 1, 4, 2, nil},
		{"leading v", "v2.0.10", 2, 0, 10, nil},
		{"all zero", "0.0.0", 0, 0, 0, nil},
		{"two parts", "1.4", 0, 0, 0, ErrMalformedVersion},
		{"four parts", "1.4.2.1", 0, 0, 0, ErrMalformedVersion},
		{"non-numeric", "1.x.2", 0, 0, 0, ErrInvalidComponent},
		{"negative", "1.-4.2", 0, 0, 0, ErrInvalidComponent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			major, minor, patch, err := ParseVersion(tc.input)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ParseVersion(%q): unexpected error: %v", tc.input, err)
				}
				if major != tc.wantMajor || minor != tc.wantMinor || patch != tc.wantPatch {
					t.Fatalf("ParseVersion(%q) = (%d, %d, %d), want (%d, %d, %d)",
						tc.input, major, minor, patch, tc.wantMajor, tc.wantMinor, tc.wantPatch)
				}
				return
			}

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseVersion(%q): err = %v, want errors.Is match for %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
```

## Review

`ParseVersion` is correct when a well-formed string yields the exact three
integers, and every malformed input lands on the right sentinel: the wrong
number of dot-separated parts is `ErrMalformedVersion`; a present but
non-numeric or negative part is `ErrInvalidComponent`. The `"leading v"` and
`"all zero"` cases matter because they are the inputs most likely to trip a
careless implementation — stripping the prefix before splitting, and not
special-casing `0` as falsy. The mistake this avoids: treating `strconv.Atoi`
failure as the only invalid case and forgetting that `-4` parses fine as an
integer but is not a valid version component.

## Resources

- [strings.TrimPrefix](https://pkg.go.dev/strings#TrimPrefix) — stripping an optional leading character without a manual index check.
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi) — string-to-int conversion, paired here with an explicit negative check `Atoi` alone does not perform.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-invoice-split-quotient-remainder.md](13-invoice-split-quotient-remainder.md) | Next: [15-tiered-config-lookup-value-found-source.md](15-tiered-config-lookup-value-found-source.md)
