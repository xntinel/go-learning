# Exercise 2: Model Observed State with Conditions and ObservedGeneration

Status is the operator's answer to "what is actually true right now, and have you
caught up to the latest spec?" This exercise builds the status machinery the
Kubernetes way: a `[]metav1.Condition` driven through the apimachinery `meta`
helpers, a `Phase` enum, and an `ObservedGeneration` that is stamped only after
convergence — with a `StatusManager` that keeps `LastTransitionTime` idempotent
and anti-flapping.

This module is self-contained and depends on `k8s.io/apimachinery`, so it is
bar-mode: built and tested where that module is available, not in the offline
gate. A clock is injected so `LastTransitionTime` assertions are deterministic.

## What you'll build

```text
cachestatus/                   module: example.com/cachestatus
  go.mod                       go 1.26; k8s.io/apimachinery
  status.go                    Phase, Status, StatusManager, condition helpers
  cmd/
    demo/
      main.go                  walk a Status Progressing -> Ready, print observed state
  status_test.go               idempotency, flip-detection, staleness (deterministic clock)
```

Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
Implement: a `StatusManager` that sets conditions through `meta.SetStatusCondition` with an injected clock, transitions Progressing/Ready/Degraded, and stamps `ObservedGeneration` only on convergence.
Test: `SetCondition` returns changed on first-set and on a status flip, false on a no-op, and only advances `LastTransitionTime` on a real flip; `IsReady` reflects the set; the staleness detector fires when `ObservedGeneration < Generation`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get k8s.io/apimachinery@v0.32.0
```

### Why the clock is injected but the condition helper is not reimplemented

The apimachinery helper `meta.SetStatusCondition(conditions *[]metav1.Condition,
newCondition metav1.Condition) bool` is the correct, standard way to mutate a
condition list, and its exact behavior is the whole point of this exercise:

- If the condition type is new, it appends it and returns `true`.
- If the type exists and the `Status` value changed, it flips the status, updates
  `LastTransitionTime`, and returns `true`.
- If the `Status` is unchanged but `Reason`, `Message`, or `ObservedGeneration`
  changed, it updates those and returns `true` — but leaves `LastTransitionTime`
  alone.
- If nothing changed, it returns `false` and touches nothing.

That "only move `LastTransitionTime` on an actual status flip" rule is what
prevents flapping, and it is why you must never hand-roll conditions with
`time.Now()`. But there is a testing wrinkle: when the helper does need to set
`LastTransitionTime` and the caller left it zero, it uses the real wall clock,
which is not deterministic. The helper has a deliberate escape hatch — if the
`newCondition` you pass already has a non-zero `LastTransitionTime`, it uses that
value instead of the wall clock. So the pattern is: our `StatusManager` stamps
`LastTransitionTime` from an injected clock *before* delegating to
`meta.SetStatusCondition`. We get deterministic timestamps for tests without
reimplementing (and breaking) the flip logic. The anti-flapping guarantee still
comes from the helper: on a no-op re-set, it ignores the fresh timestamp we passed
because the status did not change.

### The status package

`Status` mirrors the `CacheClusterStatus` subresource from Exercise 1 in miniature.
`StatusManager` carries the injected clock and exposes `SetCondition` plus three
transition helpers. `MarkProgressing` and `MarkDegraded` set their condition and
turn `Ready` off, but deliberately do *not* stamp `ObservedGeneration` — the
controller is not converged, so the observed generation must stay behind.
`MarkReady` is the only method that stamps `ObservedGeneration = generation`,
because convergence is exactly the moment the reported generation catches up.
`IsStale` implements the canonical "controller is behind" check:
`ObservedGeneration < generation`.

Create `status.go`:

```go
// Package cachestatus maintains the observed state of a custom resource using the
// Kubernetes condition and observedGeneration conventions.
package cachestatus

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is a coarse, human-facing summary of a resource's state.
type Phase string

const (
	PhasePending     Phase = "Pending"
	PhaseProgressing Phase = "Progressing"
	PhaseReady       Phase = "Ready"
	PhaseDegraded    Phase = "Degraded"
)

// Condition type names. These are the machine-facing vocabulary other controllers
// and humans key on.
const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// Status is the observed-state payload, mirroring a CRD status subresource.
type Status struct {
	Phase              Phase
	ObservedGeneration int64
	Conditions         []metav1.Condition
}

// Clock returns the current time; injected so LastTransitionTime is deterministic
// in tests.
type Clock func() time.Time

// StatusManager transitions a Status through conditions and observed generation.
type StatusManager struct {
	now Clock
}

// NewStatusManager returns a manager using the given clock, defaulting to
// time.Now when nil.
func NewStatusManager(now Clock) *StatusManager {
	if now == nil {
		now = time.Now
	}
	return &StatusManager{now: now}
}

// SetCondition stamps LastTransitionTime from the injected clock (if the caller
// left it zero) and delegates to meta.SetStatusCondition, which moves the
// timestamp only when Status actually flips. It reports whether anything changed.
func (m *StatusManager) SetCondition(s *Status, c metav1.Condition) bool {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.NewTime(m.now())
	}
	return meta.SetStatusCondition(&s.Conditions, c)
}

// MarkProgressing records that the controller is reconciling toward generation
// gen. It does NOT stamp ObservedGeneration: the controller has not converged.
func (m *StatusManager) MarkProgressing(s *Status, gen int64, reason, message string) {
	s.Phase = PhaseProgressing
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gen,
		Reason:             reason,
		Message:            message,
	})
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gen,
		Reason:             reason,
		Message:            message,
	})
}

// MarkReady records convergence for generation gen. This is the only method that
// stamps ObservedGeneration, because convergence is when the reported generation
// catches up to the spec.
func (m *StatusManager) MarkReady(s *Status, gen int64) {
	s.Phase = PhaseReady
	s.ObservedGeneration = gen
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gen,
		Reason:             "Converged",
		Message:            "all members are serving",
	})
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gen,
		Reason:             "Converged",
		Message:            "reconcile complete",
	})
	meta.RemoveStatusCondition(&s.Conditions, ConditionDegraded)
}

// MarkDegraded records a failure. It turns Ready off and does NOT stamp
// ObservedGeneration.
func (m *StatusManager) MarkDegraded(s *Status, gen int64, reason, message string) {
	s.Phase = PhaseDegraded
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gen,
		Reason:             reason,
		Message:            message,
	})
	m.SetCondition(s, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gen,
		Reason:             reason,
		Message:            message,
	})
}

// IsReady reports whether the Ready condition is currently True.
func IsReady(s *Status) bool {
	return meta.IsStatusConditionTrue(s.Conditions, ConditionReady)
}

// IsStale reports whether the controller is behind the given generation, i.e. it
// has not yet recorded convergence for the latest spec.
func IsStale(s *Status, generation int64) bool {
	return s.ObservedGeneration < generation
}
```

### The runnable demo

The demo walks a `Status` through a realistic lifecycle against a fixed clock so
its output is deterministic: a spec at generation 4 arrives, the controller marks
it Progressing (still stale, Ready=false), then converges and marks it Ready
(no longer stale, `ObservedGeneration` now 4).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/cachestatus"
)

func main() {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := cachestatus.NewStatusManager(func() time.Time { return fixed })

	s := &cachestatus.Status{Phase: cachestatus.PhasePending}
	const gen = 4

	m.MarkProgressing(s, gen, "Reconciling", "provisioning members")
	fmt.Printf("phase=%s ready=%v stale=%v observed=%d\n",
		s.Phase, cachestatus.IsReady(s), cachestatus.IsStale(s, gen), s.ObservedGeneration)

	m.MarkReady(s, gen)
	fmt.Printf("phase=%s ready=%v stale=%v observed=%d\n",
		s.Phase, cachestatus.IsReady(s), cachestatus.IsStale(s, gen), s.ObservedGeneration)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
phase=Progressing ready=false stale=true observed=0
phase=Ready ready=true stale=false observed=4
```

### Tests

The tests pin the three behaviors that make status trustworthy, all with an
injected clock so timestamps are exact. `TestSetConditionIdempotency` sets `Ready`
once (changed, timestamp at t0), re-sets the identical condition at a later clock
(no change, timestamp stays t0 — the anti-flapping guarantee), then flips it to
`False` at t2 (changed, timestamp now advances to t2). `TestIsReady` checks the
helper reflects the current set. `TestStaleness` drives a full transition and
asserts `IsStale` is true while Progressing and false after `MarkReady` stamps the
observed generation.

Create `status_test.go`:

```go
package cachestatus

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSetConditionIdempotency(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t0.Add(2 * time.Hour)

	clock := t0
	m := NewStatusManager(func() time.Time { return clock })
	s := &Status{}

	// First set: changed, timestamp at t0.
	if changed := m.SetCondition(s, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Up"}); !changed {
		t.Fatal("first SetCondition returned changed=false")
	}
	got := m.condTime(t, s, ConditionReady)
	if !got.Equal(t0) {
		t.Fatalf("LastTransitionTime = %v; want %v", got, t0)
	}

	// No-op re-set at t1: not changed, timestamp must stay t0.
	clock = t1
	if changed := m.SetCondition(s, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Up"}); changed {
		t.Fatal("no-op SetCondition returned changed=true")
	}
	if got := m.condTime(t, s, ConditionReady); !got.Equal(t0) {
		t.Fatalf("no-op moved LastTransitionTime to %v; want %v", got, t0)
	}

	// Status flip at t2: changed, timestamp advances to t2.
	clock = t2
	if changed := m.SetCondition(s, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "Down"}); !changed {
		t.Fatal("status flip returned changed=false")
	}
	if got := m.condTime(t, s, ConditionReady); !got.Equal(t2) {
		t.Fatalf("flip LastTransitionTime = %v; want %v", got, t2)
	}
}

// condTime is a test helper returning a condition's LastTransitionTime.
func (m *StatusManager) condTime(t *testing.T, s *Status, condType string) time.Time {
	t.Helper()
	for i := range s.Conditions {
		if s.Conditions[i].Type == condType {
			return s.Conditions[i].LastTransitionTime.Time
		}
	}
	t.Fatalf("condition %q not found", condType)
	return time.Time{}
}

func TestIsReady(t *testing.T) {
	t.Parallel()
	m := NewStatusManager(func() time.Time { return time.Unix(0, 0) })
	s := &Status{}

	if IsReady(s) {
		t.Fatal("empty status reported Ready")
	}
	m.MarkReady(s, 1)
	if !IsReady(s) {
		t.Fatal("MarkReady did not set Ready=True")
	}
	m.MarkDegraded(s, 2, "MembersDown", "2 of 3 unreachable")
	if IsReady(s) {
		t.Fatal("MarkDegraded left Ready=True")
	}
}

func TestStaleness(t *testing.T) {
	t.Parallel()
	m := NewStatusManager(func() time.Time { return time.Unix(0, 0) })
	s := &Status{}
	const gen = 7

	m.MarkProgressing(s, gen, "Reconciling", "in progress")
	if !IsStale(s, gen) {
		t.Fatalf("progressing status not stale: observed=%d gen=%d", s.ObservedGeneration, gen)
	}

	m.MarkReady(s, gen)
	if IsStale(s, gen) {
		t.Fatalf("converged status still stale: observed=%d gen=%d", s.ObservedGeneration, gen)
	}
	if s.ObservedGeneration != gen {
		t.Fatalf("ObservedGeneration = %d; want %d", s.ObservedGeneration, gen)
	}
}

func ExampleIsStale() {
	m := NewStatusManager(func() time.Time { return time.Unix(0, 0) })
	s := &Status{}
	m.MarkProgressing(s, 3, "Reconciling", "in progress")
	fmt.Println(IsStale(s, 3))
	m.MarkReady(s, 3)
	fmt.Println(IsStale(s, 3))
	// Output:
	// true
	// false
}
```

## Review

The status logic is correct when three invariants hold. First, idempotency:
re-setting an identical condition returns `false` and does not move
`LastTransitionTime` — this is the anti-flapping property, and it holds only
because `SetCondition` delegates to `meta.SetStatusCondition` rather than stamping
the time unconditionally. If you had written `LastTransitionTime = metav1.Now()`
by hand on every call, `TestSetConditionIdempotency` would fail on the no-op step.
Second, the timestamp advances on a genuine status flip; that is why we stamp the
injected clock before delegating — the helper honors a non-zero
`LastTransitionTime` on a flip and ignores it on a no-op. Third, and most
important for other controllers, `ObservedGeneration` is stamped only by
`MarkReady`; `MarkProgressing` and `MarkDegraded` leave it behind on purpose, so
`IsStale` reports `true` exactly when the controller has not converged to the
latest spec. The trap to avoid is stamping `ObservedGeneration` at the start of a
reconcile "to record that we saw it" — that makes `IsStale` always false and
destroys the one signal that distinguishes converged from behind.

## Resources

- [`k8s.io/apimachinery/pkg/api/meta`](https://pkg.go.dev/k8s.io/apimachinery/pkg/api/meta) — `SetStatusCondition`, `FindStatusCondition`, `IsStatusConditionTrue`, `RemoveStatusCondition`.
- [`k8s.io/apimachinery/pkg/apis/meta/v1#Condition`](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Condition) — the standard condition struct and `ConditionStatus` constants.
- [Kubernetes API conventions — Typical Status Properties](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties) — conditions, `observedGeneration`, and the meaning of transition time.

---

Back to [01-crd-api-types-and-scheme.md](01-crd-api-types-and-scheme.md) | Next: [03-operator-manager-bootstrap.md](03-operator-manager-bootstrap.md)
