# Exercise 7: Error vs Fatal and Sibling Isolation in Batch Assertions

When a CI run validates a batch, you want it to tell you about *every* broken case,
not die on the first. That is what subtests plus the right failure verb give you:
`t.Error` accumulates problems within a case and keeps going, `t.Fatal` stops only
the current subtest, and a failing subtest never fail-fasts its siblings. This
exercise builds a batch record validator that accumulates violations and tests it
so the isolation and the two verbs are load-bearing.

This module is fully self-contained: its own `go mod init`, validator, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
batchvalidate/              independent module: example.com/batchvalidate
  go.mod                    go 1.26
  validate.go               type Record; func Validate(Record) []error; sentinels
  cmd/
    demo/
      main.go               runnable demo: report every violation in a record
  validate_test.go          accumulate with t.Error; t.Fatal preconditions; isolation
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate(Record) []error` returning *every* rule violated, not just
  the first.
- Test: assert the accumulated violation set; use `t.Fatal` for the count
  precondition and `t.Errorf` per element so a run reports every mismatch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batchvalidate/cmd/demo
cd ~/go-exercises/batchvalidate
go mod init example.com/batchvalidate
```

### The two verbs and the isolation boundary

`t.Error`/`t.Errorf` mark the test failed and return normally, so control flows on
to the next statement — several `Errorf` calls in one subtest all report, giving
you every problem in that case at once. `t.Fatal`/`t.FailNow` mark the test failed
and stop it immediately by calling `runtime.Goexit` on the test goroutine — no
further statements in that subtest run. The subtest boundary contains the stop: a
`t.Fatal` in `TestValidate/case_a` ends only `case_a`; `case_b` and `case_c` still
run and still report. That is *fault isolation* — one `go test` run surfaces every
failing case in the batch instead of aborting at the first.

The rule of thumb the test below follows: use `t.Fatal` for a *precondition* whose
failure makes the rest of the subtest meaningless or unsafe (here, if the number
of returned errors is wrong, indexing them would panic), and use `t.Error` for
*independent* checks you want to keep collecting (each expected violation). The
validator mirrors the same philosophy in the domain: `Validate` returns *all*
violations of a record, not just the first, so a form can show the user every
field that is wrong in one round trip.

Create `validate.go`:

```go
package batchvalidate

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for the three rules; callers match with errors.Is.
var (
	ErrNameRequired = errors.New("name is required")
	ErrEmailInvalid = errors.New("email is invalid")
	ErrAgeNegative  = errors.New("age must not be negative")
)

// Record is one row of an inbound batch.
type Record struct {
	Name  string
	Email string
	Age   int
}

// Validate returns every rule the record violates, in a fixed order, accumulating
// problems rather than stopping at the first — the domain analogue of t.Error.
func Validate(r Record) []error {
	var errs []error
	if strings.TrimSpace(r.Name) == "" {
		errs = append(errs, ErrNameRequired)
	}
	if !strings.Contains(r.Email, "@") {
		errs = append(errs, fmt.Errorf("email %q: %w", r.Email, ErrEmailInvalid))
	}
	if r.Age < 0 {
		errs = append(errs, fmt.Errorf("age %d: %w", r.Age, ErrAgeNegative))
	}
	return errs
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchvalidate"
)

func main() {
	batch := []batchvalidate.Record{
		{Name: "Ada", Email: "ada@example.com", Age: 30},
		{Name: "  ", Email: "not-an-email", Age: -4},
	}
	for i, r := range batch {
		errs := batchvalidate.Validate(r)
		if len(errs) == 0 {
			fmt.Printf("record %d: ok\n", i)
			continue
		}
		fmt.Printf("record %d: %d violation(s)\n", i, len(errs))
		for _, e := range errs {
			fmt.Printf("  - %v\n", e)
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
record 0: ok
record 1: 3 violation(s)
  - name is required
  - email "not-an-email": email is invalid
  - age -4: age must not be negative
```

### Tests

`TestValidate` runs each record as a named subtest. Within a subtest it uses
`t.Fatalf` for the count precondition (indexing a wrong-length slice would panic,
so continuing is unsafe) and `t.Errorf` for each expected sentinel, so that if two
elements were wrong, both mismatches would be reported in one run rather than only
the first. Because each case is its own subtest, a failure in one is isolated from
the others.

Create `validate_test.go`:

```go
package batchvalidate

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   Record
		want []error // expected sentinels, in order
	}{
		{"all_valid", Record{Name: "Ada", Email: "ada@example.com", Age: 30}, nil},
		{"name_and_email", Record{Name: "   ", Email: "nope", Age: 22}, []error{ErrNameRequired, ErrEmailInvalid}},
		{"only_age", Record{Name: "Bo", Email: "bo@example.com", Age: -1}, []error{ErrAgeNegative}},
		{"all_three", Record{Name: "", Email: "x", Age: -9}, []error{ErrNameRequired, ErrEmailInvalid, ErrAgeNegative}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Validate(tc.in)

			// Precondition: a wrong count makes indexing unsafe, so stop here.
			if len(got) != len(tc.want) {
				t.Fatalf("Validate(%+v) = %d errors %v, want %d", tc.in, len(got), got, len(tc.want))
			}
			// Independent checks: t.Errorf keeps collecting, so a run reports
			// every mismatched element, not just the first.
			for i, w := range tc.want {
				if !errors.Is(got[i], w) {
					t.Errorf("error[%d] = %v, want errors.Is %v", i, got[i], w)
				}
			}
		})
	}
}

func ExampleValidate() {
	errs := Validate(Record{Name: "", Email: "bad", Age: -1})
	fmt.Println(len(errs))
	// Output: 3
}
```

### What a failing run looks like

The isolation is easiest to see by imagining a regression. Suppose `Validate`
wrongly stopped at the first violation, so `all_three` returned one error instead
of three. Only that subtest's precondition would fire; the sibling cases still run
and still report:

```text
--- FAIL: TestValidate (0.00s)
    --- FAIL: TestValidate/all_three (0.00s)
        validate_test.go:33: Validate({Name: Email:x Age:-9}) = 1 errors [name is required], want 3
    --- PASS: TestValidate/name_and_email (0.00s)
    --- PASS: TestValidate/only_age (0.00s)
    --- PASS: TestValidate/all_valid (0.00s)
FAIL
```

One run named the broken case and confirmed the others were fine — no fail-fast on
the first failure. Had the subtest used several `t.Errorf` checks that each failed,
every one would appear under that subtest rather than only the first.

## Review

The verb choice is the lesson. `t.Fatal` is for a precondition whose failure makes
the rest of the subtest meaningless or unsafe — here, a wrong error count, since
indexing past the slice would panic. `t.Error` is for independent checks you want
to keep collecting, so a case reports every problem at once. Above the subtest
level, isolation is automatic: a `t.Fatal` stops only its own subtest, so one CI
run surfaces every failing case in the batch. Do not reach for `t.Fatal` on a
routine mismatch you could keep collecting past, and do not assume the first
failure aborts the table — it does not.

## Resources

- [testing.T.Error — pkg.go.dev](https://pkg.go.dev/testing#T.Error)
- [testing.T.Fatal — pkg.go.dev](https://pkg.go.dev/testing#T.Fatal)
- [testing.T.FailNow — pkg.go.dev](https://pkg.go.dev/testing#T.FailNow)

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-parallel-goroutine-assertion-trap.md](08-parallel-goroutine-assertion-trap.md)
