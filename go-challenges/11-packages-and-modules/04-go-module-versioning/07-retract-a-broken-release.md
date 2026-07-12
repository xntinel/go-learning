# Exercise 7: Retract a broken published version and detect retractions

You published a release, then found it leaks a secret or corrupts data. You cannot
delete it and you must not force-push the tag — the checksum database already
recorded the original bytes. The sanctioned move is `retract`: a directive published
in a new, higher version that tells the toolchain to stop offering the bad release.
Here you model the directive and write the detector that answers "is this version
retracted, and why."

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
retract/                    independent module: example.com/billing/retract
  go.mod
  retract.go                Retractions() and IsRetracted() over modfile.File.Retract
  cmd/
    demo/
      main.go               runnable: classify candidate versions against a go.mod
  retract_test.go           single + interval retractions; boundary inclusivity
```

- Files: `retract.go`, `cmd/demo/main.go`, `retract_test.go`.
- Implement: `Retractions(goMod []byte)` returning the intervals, and `IsRetracted(goMod []byte, version string) (bool, string, error)` reporting membership and rationale.
- Test: `v1.2.0` and `v1.0.5` retracted (rationale surfaced), `v1.3.0` not; `Low`/`High` bounds inclusive, checked with `semver.Compare`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The retract directive, and closed-interval membership

`retract` comes in two syntactic forms, and `modfile` normalizes both into the same
structure. A single version, `retract v1.2.0`, parses into a `modfile.Retract` whose
embedded `VersionInterval` has `Low == High == "v1.2.0"`. A range, `retract [v1.0.0,
v1.1.9]`, parses into `Low == "v1.0.0"`, `High == "v1.1.9"`. Both bounds are
*inclusive* — a retraction is a closed interval `[Low, High]`. Each directive may
carry a trailing `// rationale` comment, which `modfile` captures in
`Retract.Rationale` and which `go` surfaces to anyone who tries to use the version.

Membership is therefore a pair of `semver.Compare` checks: `version` is retracted by
an interval when `Compare(version, Low) >= 0 && Compare(version, High) <= 0`. Because
`Compare` is a total order returning `-1/0/+1`, the `>= 0` at the low end and `<= 0`
at the high end are what make the interval closed — a version exactly equal to `Low`
or `High` is retracted, which is the whole point of publishing the bound. The
detector walks every retraction and returns the first that contains the candidate,
along with its rationale, so a caller learns not just *that* a version is bad but
*why*.

One property to keep straight: a retracted version still exists and still resolves if
something explicitly requires it — retraction stops the toolchain from *recommending*
or auto-selecting it, it does not erase it. That is deliberate: erasing it would break
the reproducibility of anyone who already pinned it. The correct incident response is
"publish v1.2.1 with the fix and retract v1.2.0," never "re-tag v1.2.0."

Create `retract.go`:

```go
package retract

import (
	"fmt"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Retraction is one closed interval [Low, High] with its rationale.
type Retraction struct {
	Low       string
	High      string
	Rationale string
}

// Retractions parses the retract directives from a go.mod.
func Retractions(goMod []byte) ([]Retraction, error) {
	f, err := modfile.Parse("go.mod", goMod, nil)
	if err != nil {
		return nil, fmt.Errorf("retract: parse go.mod: %w", err)
	}
	out := make([]Retraction, 0, len(f.Retract))
	for _, r := range f.Retract {
		out = append(out, Retraction{Low: r.Low, High: r.High, Rationale: r.Rationale})
	}
	return out, nil
}

// IsRetracted reports whether version falls inside any retraction interval in the
// go.mod, returning the rationale of the first matching interval.
func IsRetracted(goMod []byte, version string) (bool, string, error) {
	rs, err := Retractions(goMod)
	if err != nil {
		return false, "", err
	}
	for _, r := range rs {
		if semver.Compare(version, r.Low) >= 0 && semver.Compare(version, r.High) <= 0 {
			return true, r.Rationale, nil
		}
	}
	return false, "", nil
}
```

### The runnable demo

The demo classifies several candidate versions against a `go.mod` that retracts a
single bad release and a closed range.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/billing/retract"
)

const published = `module example.com/lib

go 1.26

retract v1.2.0 // leaked credential

retract [v1.0.0, v1.1.9] // data corruption
`

func main() {
	for _, v := range []string{"v1.3.0", "v1.2.0", "v1.0.5", "v1.0.0", "v1.1.9", "v0.9.0"} {
		bad, why, err := retract.IsRetracted([]byte(published), v)
		if err != nil {
			fmt.Println("check failed:", err)
			return
		}
		if bad {
			fmt.Printf("%-8s RETRACTED: %s\n", v, why)
		} else {
			fmt.Printf("%-8s ok\n", v)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1.3.0   ok
v1.2.0   RETRACTED: leaked credential
v1.0.5   RETRACTED: data corruption
v1.0.0   RETRACTED: data corruption
v1.1.9   RETRACTED: data corruption
v0.9.0   ok
```

### Tests

The tests prove both retraction forms and, critically, that the interval bounds are
inclusive — the `Low` and `High` versions are themselves retracted.

Create `retract_test.go`:

```go
package retract

import (
	"testing"

	"golang.org/x/mod/semver"
)

const published = `module example.com/lib

go 1.26

retract v1.2.0 // leaked credential

retract [v1.0.0, v1.1.9] // data corruption
`

func TestIsRetracted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		version   string
		want      bool
		rationale string
	}{
		{"v1.2.0", true, "leaked credential"},
		{"v1.0.5", true, "data corruption"},
		{"v1.0.0", true, "data corruption"}, // Low bound inclusive
		{"v1.1.9", true, "data corruption"}, // High bound inclusive
		{"v1.3.0", false, ""},
		{"v0.9.0", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()
			got, why, err := IsRetracted([]byte(published), tc.version)
			if err != nil {
				t.Fatalf("IsRetracted(%q): %v", tc.version, err)
			}
			if got != tc.want {
				t.Fatalf("IsRetracted(%q) = %v, want %v", tc.version, got, tc.want)
			}
			if got && why != tc.rationale {
				t.Fatalf("rationale for %q = %q, want %q", tc.version, why, tc.rationale)
			}
		})
	}
}

func TestSingleVersionInterval(t *testing.T) {
	t.Parallel()
	rs, err := Retractions([]byte(published))
	if err != nil {
		t.Fatalf("Retractions: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d retractions, want 2", len(rs))
	}
	// A single-version retract is a degenerate interval Low == High.
	single := rs[0]
	if single.Low != "v1.2.0" || single.High != "v1.2.0" {
		t.Fatalf("single retract = [%s, %s], want [v1.2.0, v1.2.0]", single.Low, single.High)
	}
}

func TestBoundInclusivityViaCompare(t *testing.T) {
	t.Parallel()
	// Low inclusive: v1.0.0 is not less than Low v1.0.0.
	if semver.Compare("v1.0.0", "v1.0.0") < 0 {
		t.Fatal("Compare says v1.0.0 < v1.0.0")
	}
	// High inclusive: v1.1.9 is not greater than High v1.1.9.
	if semver.Compare("v1.1.9", "v1.1.9") > 0 {
		t.Fatal("Compare says v1.1.9 > v1.1.9")
	}
	// Just outside: v1.2.0 is greater than High v1.1.9.
	if semver.Compare("v1.2.0", "v1.1.9") <= 0 {
		t.Fatal("Compare says v1.2.0 <= v1.1.9")
	}
}
```

## Review

The detector is correct when membership is a closed interval: `Compare(v, Low) >= 0
&& Compare(v, High) <= 0`, with both bounds inclusive because the bound versions are
the ones you are retracting. The tests pin that inclusivity directly —
`v1.0.0` and `v1.1.9` are retracted, `v1.2.0` (one patch past `High`) is not — and
prove the single-version form is the degenerate interval `Low == High`. The rationale
is surfaced so a caller learns why, which is the operational payoff over a bare
boolean. The mistake this exercise inoculates against is the force-push "fix": a
retracted version still resolves for anyone who pinned it, so retraction is additive
and safe, whereas re-tagging breaks the checksum database for every consumer.

## Resources

- [Go Modules Reference: retract directive](https://go.dev/ref/mod#go-mod-file-retract) — syntax and semantics of single and interval retractions.
- [`modfile.Retract`](https://pkg.go.dev/golang.org/x/mod/modfile#Retract) — the parsed structure with `Low`/`High`/`Rationale`.
- [Deprecating and retracting modules](https://go.dev/ref/mod#going-away) — the incident-response workflow retraction is part of.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-replace-directive-hotfix.md](08-replace-directive-hotfix.md)
