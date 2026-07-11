# Exercise 33: Schema Validation and Transformation Pipeline with Rollback

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye casos borde).

A record ingestion pipeline has two very different jobs that get
conflated far too often: deciding whether a record is acceptable at all,
and reshaping an acceptable record into its final form. `Pipeline` keeps
them as two distinct phases — `ValidateAll` accumulates every rule
violation instead of stopping at the first, and `TransformChain` only
runs once validation has fully passed, rolling back to the untouched
original the moment any transform step fails partway through.

## What you'll build

```text
schemapipe/                  independent module: example.com/schemapipe
  go.mod                     go 1.24
  schemapipe.go                type Record, Validator, Transformer; func ValidateAll, TransformChain, Pipeline
  schemapipe_test.go            error accumulation, transform ordering, rollback, no-mutation, skip-on-invalid
  cmd/demo/
    main.go                  a passing signup and a rejected-mid-chain signup
```

- Files: `schemapipe.go`, `schemapipe_test.go`, `cmd/demo/main.go`.
- Implement: `Record map[string]any`, `Validator func(data Record) error`, `Transformer func(data Record) (Record, error)`, `ValidateAll(data Record, validators ...Validator) error`, `TransformChain(data Record, transforms ...Transformer) (Record, error)`, and `Pipeline(data Record, validators []Validator, transforms []Transformer) (Record, error)`.
- Test (table plus targeted cases): `ValidateAll` reports every failing validator's error, not just the first; `TransformChain` applies steps in order; a transform failing partway through rolls back to a copy of the original, untransformed input; a zero-transform chain returns a copy of the input; `Pipeline` never runs any transform when validation fails.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/schemapipe/cmd/demo
cd ~/go-exercises/schemapipe
go mod init example.com/schemapipe
go mod edit -go=1.24
```

### Accumulate, then gate, then roll back

`ValidateAll` and `TransformChain` embody two opposite error-handling
philosophies on purpose. Validation accumulates: every `Validator` runs
against the same `data`, regardless of whether an earlier one failed,
because a caller fixing a rejected record wants to know about every
problem in one response, not one per round-trip. Transformation does the
opposite — it stops at the first failure, because transforms are
typically dependent steps (normalize, then compute a derived field from
the normalized value), and continuing past a failed step would mean
running later steps against data that never received the earlier
transformation they expected.

Rollback is what makes a mid-chain transform failure safe: `TransformChain`
works against a `clone` of `data` — call it the working copy — but the
error path returns `clone(data)` again, a *fresh* copy of the original,
not the partially-mutated working copy. Whatever the first successful
transform already changed in the working copy is discarded entirely; the
caller sees either the fully-transformed result or the untouched
original, never something in between. `Pipeline` layers one more
guarantee on top: it calls `TransformChain` at all only after `ValidateAll`
returns nil, so a chain of transforms tuned to assume validated input
never has to defend against malformed data itself.

Create `schemapipe.go`:

```go
package schemapipe

import (
	"errors"
	"fmt"
)

// Record is the document a Validator inspects and a Transformer rewrites.
// A Record is treated as immutable by convention: every function in this
// package that would otherwise mutate one instead returns a new map.
type Record map[string]any

// Validator inspects data and reports a problem with it, or nil if data
// is acceptable.
type Validator func(data Record) error

// Transformer rewrites data into a new Record, or reports an error if it
// cannot.
type Transformer func(data Record) (Record, error)

// clone makes a shallow copy of a Record so a step can be handed a
// working copy without risking mutation of the caller's original map.
func clone(r Record) Record {
	out := make(Record, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

// ValidateAll runs every validator against data and accumulates every
// failure — unlike a fail-fast chain, one validator rejecting the record
// does not stop the others from also reporting their own problems, so a
// caller sees every violation in a single response instead of fixing
// issues one round-trip at a time.
func ValidateAll(data Record, validators ...Validator) error {
	var errs []error
	for _, validate := range validators {
		if err := validate(data); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// TransformChain runs transforms in order against a working copy of
// data. If any transform fails, the whole chain rolls back: it returns a
// copy of the original, untransformed data rather than whatever partial
// state the earlier, successful transforms produced.
func TransformChain(data Record, transforms ...Transformer) (Record, error) {
	current := clone(data)
	for i, transform := range transforms {
		next, err := transform(current)
		if err != nil {
			return clone(data), fmt.Errorf("transform %d: %w", i, err)
		}
		current = next
	}
	return current, nil
}

// Pipeline validates data against every validator, accumulating all
// failures, and only runs the transform chain if validation passed
// entirely. Either a validation failure or a transform failure returns a
// copy of the original, untouched data alongside the error.
func Pipeline(data Record, validators []Validator, transforms []Transformer) (Record, error) {
	if err := ValidateAll(data, validators...); err != nil {
		return clone(data), err
	}
	return TransformChain(data, transforms...)
}
```

### The runnable demo

The demo runs the same two-validator, two-transform pipeline against two
signups: one that clears every rule, and one that fails the
age-restriction transform after the email-normalization transform has
already run — showing that the successful step's partial work never
leaks into the rolled-back result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/schemapipe"
)

func requireField(field string) schemapipe.Validator {
	return func(data schemapipe.Record) error {
		if _, ok := data[field]; !ok {
			return fmt.Errorf("missing required field %q", field)
		}
		return nil
	}
}

func lowercaseEmail(data schemapipe.Record) (schemapipe.Record, error) {
	email, _ := data["email"].(string)
	data["email"] = strings.ToLower(email)
	return data, nil
}

func rejectMinors(data schemapipe.Record) (schemapipe.Record, error) {
	age, _ := data["age"].(int)
	if age < 18 {
		return nil, errors.New("age below minimum of 18")
	}
	data["status"] = "active"
	return data, nil
}

func run(label string, record schemapipe.Record) {
	validators := []schemapipe.Validator{requireField("email"), requireField("age")}
	transforms := []schemapipe.Transformer{lowercaseEmail, rejectMinors}

	result, err := schemapipe.Pipeline(record, validators, transforms)
	fmt.Printf("%s: original=%v\n", label, record)
	fmt.Printf("%s: result=%v err=%v\n", label, result, err)
}

func main() {
	run("adult signup", schemapipe.Record{"email": "USER@Example.com", "age": 30})
	run("minor signup", schemapipe.Record{"email": "KID@Example.com", "age": 15})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
adult signup: original=map[age:30 email:USER@Example.com]
adult signup: result=map[age:30 email:user@example.com status:active] err=<nil>
minor signup: original=map[age:15 email:KID@Example.com]
minor signup: result=map[age:15 email:KID@Example.com] err=transform 1: age below minimum of 18
```

The minor signup's `result` still shows the original, uppercase email —
`lowercaseEmail` did run successfully on the internal working copy before
`rejectMinors` failed, but that partial progress is exactly what
rollback discards. (`fmt`'s `%v` on a map prints keys in sorted order,
which is why this output is stable across runs despite Go's map
iteration itself being unordered.)

### Tests

`TestValidateAllAccumulatesEveryFailure` is the property that
distinguishes validation from transformation in this package: both
missing fields are reported, not just the first one encountered.
`TestTransformChainAppliesStepsInOrder` establishes the baseline for
rollback to be measured against. `TestTransformChainRollsBackOnFailureMidway`
is the core of the exercise — it runs a chain where the first step
succeeds and the second fails, then asserts the result equals the
*original* input, not the partially-transformed working copy, and
separately asserts the original `Record` passed in was itself never
mutated. `TestTransformChainWithNoTransformsReturnsCopyOfInput` covers
the degenerate zero-step chain. `TestPipelineSkipsTransformsWhenValidationFails`
proves the ordering guarantee between the two phases: a transform with a
side effect (`transformCalled`) never fires when validation rejects the
record first.

Create `schemapipe_test.go`:

```go
package schemapipe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func requireField(field string) Validator {
	return func(data Record) error {
		if _, ok := data[field]; !ok {
			return errors.New("missing " + field)
		}
		return nil
	}
}

func TestValidateAllAccumulatesEveryFailure(t *testing.T) {
	t.Parallel()

	err := ValidateAll(Record{}, requireField("email"), requireField("age"))
	if err == nil {
		t.Fatal("ValidateAll() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "missing email") || !strings.Contains(err.Error(), "missing age") {
		t.Fatalf("err = %v, want it to mention both missing fields", err)
	}
}

func TestValidateAllPassesWhenEveryValidatorPasses(t *testing.T) {
	t.Parallel()

	err := ValidateAll(Record{"email": "a@b.com", "age": 30}, requireField("email"), requireField("age"))
	if err != nil {
		t.Fatalf("ValidateAll() = %v, want nil", err)
	}
}

func upper(data Record) (Record, error) {
	name, _ := data["name"].(string)
	data["name"] = name + "-upper"
	return data, nil
}

func failIfMissing(field string) Transformer {
	return func(data Record) (Record, error) {
		if _, ok := data[field]; !ok {
			return nil, errors.New("cannot transform: missing " + field)
		}
		return data, nil
	}
}

func TestTransformChainAppliesStepsInOrder(t *testing.T) {
	t.Parallel()

	original := Record{"name": "base"}
	result, err := TransformChain(original, upper, upper)
	if err != nil {
		t.Fatalf("TransformChain() err = %v, want nil", err)
	}
	if result["name"] != "base-upper-upper" {
		t.Fatalf(`result["name"] = %v, want "base-upper-upper"`, result["name"])
	}
}

func TestTransformChainRollsBackOnFailureMidway(t *testing.T) {
	t.Parallel()

	original := Record{"name": "base"}
	originalCopy := Record{"name": "base"}

	result, err := TransformChain(original, upper, failIfMissing("nonexistent"), upper)
	if err == nil {
		t.Fatal("TransformChain() err = nil, want an error from the second step")
	}
	if !reflect.DeepEqual(result, originalCopy) {
		t.Fatalf("result = %v, want the original untransformed record %v (rollback failed)", result, originalCopy)
	}
	if !reflect.DeepEqual(original, originalCopy) {
		t.Fatalf("original = %v, want unmutated %v (TransformChain must not mutate its input)", original, originalCopy)
	}
}

func TestTransformChainWithNoTransformsReturnsCopyOfInput(t *testing.T) {
	t.Parallel()

	original := Record{"name": "base"}
	result, err := TransformChain(original)
	if err != nil {
		t.Fatalf("TransformChain() err = %v, want nil", err)
	}
	if !reflect.DeepEqual(result, original) {
		t.Fatalf("result = %v, want %v", result, original)
	}
}

func TestPipelineSkipsTransformsWhenValidationFails(t *testing.T) {
	t.Parallel()

	transformCalled := false
	transform := func(data Record) (Record, error) {
		transformCalled = true
		return data, nil
	}

	original := Record{"name": "base"}
	result, err := Pipeline(original, []Validator{requireField("email")}, []Transformer{transform})
	if err == nil {
		t.Fatal("Pipeline() err = nil, want a validation error")
	}
	if transformCalled {
		t.Fatal("a transform ran despite failed validation")
	}
	if !reflect.DeepEqual(result, original) {
		t.Fatalf("result = %v, want the untouched original %v", result, original)
	}
}

func TestPipelineRunsTransformsWhenValidationPasses(t *testing.T) {
	t.Parallel()

	original := Record{"name": "base", "email": "a@b.com"}
	result, err := Pipeline(original, []Validator{requireField("email")}, []Transformer{upper})
	if err != nil {
		t.Fatalf("Pipeline() err = %v, want nil", err)
	}
	if result["name"] != "base-upper" {
		t.Fatalf(`result["name"] = %v, want "base-upper"`, result["name"])
	}
}
```

## Review

`TransformChain` is correct because its error path returns `clone(data)`
— a fresh copy of the *original* argument — rather than `current`, the
mutated working copy the failed step was handed. Returning `current` on
error is the bug this exercise exists to catch: it looks almost right in
a quick test with a single transform, and only breaks visibly once a
chain has at least two steps and the failure happens after the first one
succeeds, which is exactly why `TestTransformChainRollsBackOnFailureMidway`
uses three transforms with the failure in the middle rather than a
one-step chain. `ValidateAll`'s accumulation and `TransformChain`'s
fail-fast are not inconsistent — they are the right default for their
respective jobs, and `Pipeline`'s ordering (validate fully, then
transform) is what lets each phase assume the other has already done its
job.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Join`, the accumulation mechanism behind `ValidateAll`.
- [maps.Clone](https://pkg.go.dev/maps#Clone) — a standard-library equivalent of this package's hand-written `clone` helper.
- [JSON Schema validation](https://json-schema.org/understanding-json-schema/reference/annotations) — a widely used schema-validation model built on the same "collect every violation" philosophy as `ValidateAll`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-time-series-percentile-reducer.md](32-time-series-percentile-reducer.md) | Next: [34-request-correlation-id-propagator.md](34-request-correlation-id-propagator.md)
