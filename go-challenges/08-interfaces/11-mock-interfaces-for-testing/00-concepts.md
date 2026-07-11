# Test Doubles for Interfaces — Concepts

Every service layer in a backend sits behind ports: a repository, a cache, a
message broker, a payment gateway, an outbound HTTP API, a clock. The unit tests
for that layer live or die by how those seams are doubled. Get the double right
and the test is fast, deterministic, and pins exactly the contract you care
about. Get it wrong and you ship either a brittle test that breaks on every
harmless refactor, or a vacuous one that is green while production is broken.
This file is the conceptual foundation for the nine independent exercises that
follow; each builds one real double against one real port.

## Concepts

### The test-double taxonomy: dummy, stub, spy, mock, fake

Gerard Meszaros' vocabulary, popularized by Martin Fowler, distinguishes five
kinds of test double, and the distinction is not pedantry — each has a distinct
job and a distinct failure mode, and picking the wrong one is the root cause of
most brittle or meaningless tests.

A *dummy* is a value passed only to satisfy a signature; it is never actually
used. A *stub* provides canned answers: `FindByID` always returns the same user,
or `Save` always returns a configured error. A stub feeds the system under test
(SUT) predetermined inputs so you can drive it down a chosen branch. A *spy*
records how it was called — the arguments, the order, the count — so the test can
assert on that record afterward. A *mock* is a spy with pre-programmed
expectations that *self-verify*: you tell it up front "expect `Authorize` exactly
once with this amount, before `Capture`", and the mock fails the test itself if
the interaction does not match. A *fake* is a lightweight but genuinely working
implementation: an in-memory map standing in for a database, a `net.Pipe` for a
socket. A fake has real behavior — write then read returns what you wrote — which
a stub or spy does not.

The practical rule: use a *stub* when you need to steer the SUT down an error or
success branch; a *spy* when you need to assert the SUT called a collaborator
correctly; a *fake* when the SUT does a multi-call round-trip (read-after-write,
create-then-list) that a canned answer cannot honestly model; and a *mock* when
the exact interaction — order, count, arguments — is itself the contract under
test. Colloquially the whole family is called "mocks", but the differences drive
real design decisions.

### State-based versus interaction-based verification

There are two fundamentally different things a test can assert. *State-based*
verification exercises the SUT and then asserts on the resulting state: the value
the method returned, the entries a fake now holds, the slice of values a spy
recorded. *Interaction-based* (behavior) verification asserts on the calls
themselves: that `Save` was invoked exactly twice, with these arguments, in this
order. Hand-rolled spies and fakes naturally favor state verification; gomock and
testify expectations are built for interaction verification.

The trade-off is coupling. Interaction verification couples the test to *how* the
SUT does its job, not just *what* it produces. A test that asserts "calls
`Save(1)` then `Save(2)`" breaks the moment you refactor the SUT to batch both
into one `SaveAll([1,2])` call — even though the observable outcome is identical
and correct. Over-specified interaction tests are the classic source of a suite
that goes red on every harmless change. The discipline: assert the *minimal*
observable contract. Prefer state verification when call order is irrelevant.
Reach for strict interaction verification only when the order and count are
themselves the thing that must be guaranteed (Authorize strictly before Capture;
publish exactly once for idempotency).

### Accept interfaces, return structs, define the interface at the consumer

Go's idiom differs sharply from the Java tradition of declaring an interface
next to its implementation. In Go you define the interface at the *consumer* —
the package that needs the dependency — and keep it as small as that caller
actually uses, often a single method. The producer returns a concrete struct and
declares no interface at all.

This is exactly what makes doubles cheap. If `Counter` needs only
`Save(int64) error`, it declares a one-method `Storage` interface, and the mock
is three lines. If instead you mocked the producer's full `*sql.DB`-shaped type,
the double would need dozens of methods the SUT never calls. A small
consumer-defined port yields a tiny double and decouples the test from the
producer's entire surface. "Accept interfaces, return structs" is not a style
preference here; it is what makes the seam mockable at all.

### Mockability is a design signal

If a unit is painful to mock, that is information about the design, not the test.
Usually it means the dependency is a concrete type with no seam, or the interface
is too fat. The fix is to inject the collaborator behind a small interface so the
seam exists. Time, randomness, and the network are the three classic *hidden*
dependencies: code that calls `time.Now()`, `rand.Int()`, or `http.Get()`
directly has smuggled in a dependency you cannot control from a test. Hoist each
behind an interface — a `Clock`, a source of randomness, an `http.RoundTripper`
— and the unit becomes deterministic and fast.

### Deterministic time and I/O

Two dependencies flake more tests than any other: the wall clock and the network.
A retry-with-backoff that calls `time.Sleep` really sleeps; a test of three
retries with exponential backoff waits real seconds and is still racy at the
boundaries. Replace `time.Now`/`time.Sleep` with a `Clock` interface and a test
supplies a fake clock that advances instantly and exactly, so a backoff schedule
is asserted in microseconds with zero wall-clock waiting. (Go 1.25's
`testing/synctest` offers an alternative that virtualizes real `time` calls
inside a bubble; the `Clock` interface remains the portable, production-usable
technique and is what these exercises teach.) For the outbound network, inject an
`http.RoundTripper` into the `*http.Client`: the test returns canned
`*http.Response` values and captures the outgoing `*http.Request`, testing the
client's URL-building, header, and status-handling logic with no socket.

### Concurrency-safe doubles

A double is production-shaped code, and it must be correct under the same
concurrency the SUT imposes on it. A spy whose recorded slice is appended to from
many goroutines has a data race in the *test* — and the `-race` detector will,
correctly, flag it. Guard the recorded state with a mutex, and return a defensive
copy from the accessor so a caller iterating the record cannot race a concurrent
append. A double that races is not a testing detail to wave away; it is a real
bug in real code that happens to live in `_test.go`.

### Generated versus hand-rolled doubles

Hand-rolled doubles are more lines but zero magic: a struct, a method, a recorded
slice, an assertion. For a small, stable, one- or two-method port they are the
clearest choice. For a large or churning interface, the boilerplate dominates and
generated mocks earn their keep. `go.uber.org/mock` (the maintained fork; the
original `github.com/golang/mock` is archived and should not be used in new code)
generates a mock from an interface via `mockgen` and gives you EXPECT-style
expectations, argument matchers (`gomock.Any`, `gomock.Eq`, `gomock.Cond`), call
counts (`Times`), and ordering (`gomock.InOrder`). `github.com/stretchr/testify/
mock` takes a different, reflection-driven approach: `m.On("Send",
mock.MatchedBy(...)).Return(nil).Once()` and `AssertExpectations`. Both cut
boilerplate at the cost of readability, a codegen or reflection step, and a
dependency. Choose per interface size and churn — there is no single right
answer, and a mature codebase mixes hand-rolled doubles for small ports with
generated ones for the big volatile services.

### Argument matchers and call counts are coupling

Matchers and counts — `gomock.Any`/`Eq`/`InOrder`, testify's `mock.Anything`/
`MatchedBy`/`Once`/`Times`/`AssertNumberOfCalls` — let you express precise
expectations. Precision is a tool, not a virtue. Every constraint you add is a
line of coupling between the test and the implementation. Assert only what the
contract *truly* requires: if the payload must contain a specific order ID, match
on that field with `MatchedBy`; do not also pin the exact number of retries the
SUT happens to make today unless retry-count is part of the contract.

### Mocks encode your assumptions, not the dependency's truth

The deepest limitation: a mock only ever asserts the contract *you* encoded into
it. It never proves the real dependency honors that contract. A mock configured
to return `{"user_id": 7}` stays green forever, even after the real API renames
the field to `userId` or changes an error's semantics. A suite of all-green mocks
can ship a completely broken integration — the mock lies, faithfully, about a
world that has moved on. The mitigation is not to abandon mocks but to pair them:
keep the fast mock-based unit tests, and add at least one integration or contract
test per port that exercises the real thing — an `httptest.NewServer` for an HTTP
client, a real (or containerized) database for a repository. The mocks give you
speed and branch coverage; the contract test catches the day the dependency's
truth diverges from your assumption.

## Common Mistakes

### Mocking a fat interface you do not own

Wrong: mocking a concrete type or a wide third-party interface, so the double has
methods the SUT never calls. The test compiles and passes while asserting nothing
meaningful about the real dependency.

Fix: define a minimal interface at the consumer — often one method — and double
exactly that.

### Injecting a spy and never asserting on it

Wrong: wiring a spy into the SUT and then never checking its recorded calls. A
spy with no assertion is a no-op that manufactures false confidence.

Fix: assert on the call record (state-based) or set and verify expectations
(gomock's auto-`Finish`, testify's `AssertExpectations`).

### Over-specifying the interaction

Wrong: asserting exact call order, counts, and arguments the contract does not
actually require, so the test breaks on every harmless refactor (e.g. batching
two saves into one).

Fix: assert the minimal observable contract; prefer state verification when order
is irrelevant.

### Sharing a double at package scope

Wrong: a package-level mock reused across tests, causing cross-test state
pollution and order-dependent flakiness.

Fix: instantiate the double inside each test or subtest; use `t.Cleanup` for
teardown.

### A double with an unsynchronized shared record

Wrong: a spy appended from multiple goroutines with no lock — the double itself
has a data race, which `-race` flags.

Fix: guard the recorded state with a mutex and return a defensive copy from the
accessor.

### Calling time.Now/time.Sleep or the real network inside the unit

Wrong: the unit calls `time.Now`, `time.Sleep`, or hits a live URL, making the
test slow, flaky, and nondeterministic.

Fix: inject a `Clock` and an `http.RoundTripper` (or an `httptest` server) and
control them from the test.

### Trusting a mock-only suite

Wrong: relying entirely on mocks, which stay green after the real dependency's
contract changes (a renamed field, different error semantics).

Fix: keep one integration or contract test per port alongside the mock-based unit
tests.

### Reaching for the archived gomock

Wrong: importing `github.com/golang/mock` (archived) or hand-writing mocks for a
large, volatile interface.

Fix: use the maintained `go.uber.org/mock` with `mockgen`, or `testify/mock`, for
big interfaces; keep hand-rolled doubles for small stable ports.

### Forgetting the response Body lifecycle in a fake RoundTripper

Wrong: returning an `*http.Response` whose `Body` is nil or a bare
`*strings.Reader`, so the client panics on `Body.Close()`.

Fix: set `Body: io.NopCloser(strings.NewReader(...))` and honor the same
lifecycle the real client expects.

### Returning a shared mutable slice or map from a double

Wrong: a stub or fake returns its internal slice/map, which the SUT then mutates,
corrupting later assertions.

Fix: return copies from the double and reset it per test.

Next: [01-hand-rolled-spy-counter.md](01-hand-rolled-spy-counter.md)
