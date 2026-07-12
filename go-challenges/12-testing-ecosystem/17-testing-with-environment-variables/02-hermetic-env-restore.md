# Exercise 2: Prove Your Env Tests Are Hermetic And Do Not Leak

A test that mutates the environment must leave it exactly as it found it, or it
poisons every sibling that reads the same variable. This exercise makes that
property — hermeticity — an executable contract: snapshot the original with
`os.LookupEnv`, assert restoration inside a `t.Cleanup`, and prove the cleanup
ordering that `t.Setenv` relies on.

## What you'll build

```text
envprobe/                  independent module: example.com/envprobe
  go.mod                   go directive supplied by the gate
  region.go                Region() reads APP_REGION via os.LookupEnv, defaults us-east-1
  cmd/
    demo/
      main.go              runnable demo: default, then override
  region_test.go           hermeticity assertion in Cleanup; LIFO cleanup-order proof
```

Files: `region.go`, `cmd/demo/main.go`, `region_test.go`.
Implement: `Region() string` returning `APP_REGION` when set, else `"us-east-1"`.
Test: snapshot with `os.LookupEnv`, assert-in-`Cleanup` that the value is restored (covering both was-set and was-unset), and prove `t.Cleanup` runs LIFO.
Verify: `go test -count=1 -race ./...`

## Why hermeticity is a property you assert

The original version of this lesson proved restoration by hand-walking
`os.Environ()` — a slice of `"KEY=VALUE"` strings — and splitting each entry on
`=`. That works, but it is exactly the kind of manual parsing `os.LookupEnv`
exists to replace: `os.LookupEnv(key)` returns `(value, ok)` directly, where `ok`
distinguishes "set" from "unset". The modern, correct snapshot is one line.

The subtle part is the snapshot must capture *both* the value and whether the
variable was set at all, because restoration means returning to the exact prior
state. If `APP_REGION` was unset before the test, hermetic restoration means it
is unset again afterward — not set to `""`. Capturing `(orig, hadOrig)` and
asserting both in the cleanup covers that case without assuming anything about
the ambient environment, so the test is correct whether or not CI happens to have
`APP_REGION` set.

`t.Setenv` already does this restoration for you; the point of the test is to
*prove* it does, turning an invisible guarantee into a visible check. If someone
later refactors the code to use `os.Setenv` without cleanup, this test fails in
the test that caused the leak, not as a flake somewhere downstream.

Create `region.go`:

```go
package envprobe

import "os"

// Region returns the deployment region from APP_REGION, or a default when the
// variable is unset. It uses os.LookupEnv so an explicitly empty APP_REGION is
// treated as set (returning "") rather than silently defaulting.
func Region() string {
	if v, ok := os.LookupEnv("APP_REGION"); ok {
		return v
	}
	return "us-east-1"
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"example.com/envprobe"
)

func main() {
	os.Unsetenv("APP_REGION")
	fmt.Println("default:", envprobe.Region())

	os.Setenv("APP_REGION", "eu-west-1")
	fmt.Println("override:", envprobe.Region())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default: us-east-1
override: eu-west-1
```

## Tests

`TestSetenvRestores` snapshots `(orig, hadOrig)` with `os.LookupEnv`, registers a
`t.Cleanup` that asserts the variable is back to exactly that state, then calls
`t.Setenv` and confirms the temporary value is visible mid-test. Because the
cleanup runs after the test body returns, the assertion inside it is what proves
restoration — a comment could not.

`TestCleanupLIFO` proves the ordering `t.Setenv` depends on: cleanups run
last-added-first-called. The trick to observe it is a subtest — `t.Run` blocks
until the subtest *and its cleanups* finish, so after the subtest returns the
parent can assert the recorded order. Three cleanups append `1, 2, 3` in
registration order and therefore run `3, 2, 1`.

Create `region_test.go`:

```go
package envprobe

import (
	"os"
	"slices"
	"testing"
)

func TestSetenvRestores(t *testing.T) {
	const key = "APP_REGION"

	// Snapshot the exact prior state: value and whether it was set at all.
	orig, hadOrig := os.LookupEnv(key)

	t.Cleanup(func() {
		got, has := os.LookupEnv(key)
		if has != hadOrig || got != orig {
			t.Fatalf("env %s not restored: got (%q,%v), want (%q,%v)", key, got, has, orig, hadOrig)
		}
	})

	t.Setenv(key, "eu-west-1")
	if got := Region(); got != "eu-west-1" {
		t.Fatalf("Region() during test = %q, want eu-west-1", got)
	}
}

func TestCleanupLIFO(t *testing.T) {
	var order []int
	t.Run("inner", func(t *testing.T) {
		t.Cleanup(func() { order = append(order, 1) })
		t.Cleanup(func() { order = append(order, 2) })
		t.Cleanup(func() { order = append(order, 3) })
	})
	// The subtest's cleanups have all run by now, last-added first.
	want := []int{3, 2, 1}
	if !slices.Equal(order, want) {
		t.Fatalf("cleanup order = %v, want %v (LIFO)", order, want)
	}
}
```

The runnable `Example` lives in its own file so the environment is set and unset
explicitly, giving deterministic output.

Create `region_example_test.go`:

```go
package envprobe

import (
	"fmt"
	"os"
)

func ExampleRegion() {
	os.Unsetenv("APP_REGION")
	fmt.Println(Region())

	os.Setenv("APP_REGION", "ap-south-1")
	fmt.Println(Region())

	os.Unsetenv("APP_REGION")
	// Output:
	// us-east-1
	// ap-south-1
}
```

## Review

Hermeticity holds when the cleanup assertion sees the variable back in its exact
prior state — the same value and the same set/unset status captured by
`os.LookupEnv` before the mutation. The reason to snapshot `(orig, hadOrig)`
rather than a bare string is the unset case: restoring an originally-unset
variable to `""` would be a leak, and only the `ok` flag catches it. The LIFO
test matters because `t.Setenv`'s restoration is itself a registered cleanup, and
when several cleanups touch the same state, order decides the outcome — proving
`3, 2, 1` is proving the mechanism you are trusting.

## Resources

- [os.LookupEnv](https://pkg.go.dev/os#LookupEnv) — returns `(value, ok)`, distinguishing unset from empty.
- [testing.T.Cleanup](https://pkg.go.dev/testing#T.Cleanup) — cleanups run last-added-first-called.
- [os.Environ](https://pkg.go.dev/os#Environ) — the raw `KEY=VALUE` slice `LookupEnv` saves you from parsing.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-env-config-loader.md](01-env-config-loader.md) | Next: [03-required-vs-optional-lookupenv.md](03-required-vs-optional-lookupenv.md)
