# Mocking, Test Doubles, and Dependency Isolation for Backend Services — Concepts

A production backend service is never alone. It talks to a database, a payment
gateway, an SMS provider, a message broker, an object store, and the wall clock.
Every one of those collaborators is slow, non-deterministic, costly, or all
three, and none of them belongs in the hot path of a unit test. The senior skill
this lesson teaches is not "how to use a mocking framework"; it is how to design
*seams* so the code you own can be tested deterministically and in isolation from
the collaborators you do not control, and — just as important — knowing which
kind of double to reach for and when to stop reaching for one at all. Over-mocking
is a real and common failure: it produces a green test suite sitting on top of a
broken system, because the tests assert on call sequences the implementation
happens to make rather than on the outcomes the user actually cares about.

This file is the conceptual foundation. Read it once and you have the model you
need for all ten independent exercises that follow: a hand-rolled spy, the full
test-double taxonomy against a repository, a function-field stub, gomock (both
plain generated mocks and ordering/matchers), testify/mock, an `http.RoundTripper`
fake, an injected `Clock`, consumer-defined interface segregation, and go-sqlmock
at the driver boundary.

## Concepts

### The test-double taxonomy (Meszaros / Fowler)

"Mock" is the word people use for all five of these, and conflating them is the
root of most bad test design. Gerard Meszaros coined precise names, popularized
by Martin Fowler; they are distinct tools with distinct failure modes.

- A **dummy** fills a required parameter slot and is never actually used. You pass
  it because the signature demands a value, not because the code will call it. A
  `context.Context` you pass only to satisfy an argument, or a logger a code path
  never reaches, is a dummy.
- A **stub** returns canned answers. It has no logic beyond "when asked, reply
  with this preprogrammed value." A `GetByEmail` that always returns the same user
  (or always `ErrNotFound`) is a stub. Stubs enable a code path; they do not verify
  anything.
- A **spy** is a stub that *also records how it was called*. It answers like a
  stub and captures the arguments and call count so a test can inspect them
  afterward. The hand-rolled `mockSender` in Exercise 1 is a spy: it returns a
  canned error and records every `{to, body}` it received.
- A **fake** is a working, lightweight implementation of the real thing. An
  in-memory map-backed repository with genuine uniqueness semantics is a fake:
  `Save` then `GetByEmail` really returns what you saved, and a duplicate really
  fails. Fakes have behavior, so tests can do pure state verification against them
  with no assertions about calls at all.
- A **mock** is preprogrammed with *expectations* and verifies itself. You tell it
  "expect `Charge` to be called exactly once with these args, and return this,"
  and at the end it fails the test if the expectation was not met (or if an
  unexpected call arrived). gomock and testify/mock generate or provide this
  machinery.

The taxonomy is not academic. The wrong choice couples your test to the wrong
thing. Reach for a fake or a stub by default; reach for a spy when you must
inspect what was passed; reach for a self-verifying mock only when the *interaction
itself is the contract*.

### State verification versus interaction verification

There are two fundamentally different things a test can assert.

**State verification** asserts on the observable *result*: the value the function
returned, or the final contents of a fake. "After `SignUp`, `GetByEmail` returns
the normalized user" is state verification. It says nothing about how the service
achieved that; it is robust to refactoring because it only pins down observable
behavior.

**Interaction verification** asserts that a collaborator was *called* in a
particular way: "`repo.Save` was called exactly once," "`Publish` happened before
`MarkPublished`." It reaches inside the implementation and pins down its
collaboration protocol.

Prefer state verification. It is the default because it survives refactors: you
can rewrite the internals, and as long as the outcome is the same the test still
passes. Reserve interaction verification for the cases where the interaction *is*
the observable contract and there is no state to inspect:

- **Exactly-once semantics.** For idempotency you may need to prove the payment
  gateway was charged exactly once across a retry — there is no state on your side
  that captures "charged twice," only the number of calls.
- **No-call-on-failure.** A validation failure must short-circuit *before* the
  expensive collaborator runs; the contract is "the sender was not called at all."
- **Ordering across a boundary.** A transactional outbox must `Publish` before it
  `MarkPublished`, per message; the order of the two calls is the correctness
  property.

If you find yourself writing interaction assertions for anything else — asserting
`Save` was called but never checking *what* was saved — you are testing the
implementation, not the behavior, and the test will lie to you: it stays green
while the handler persists garbage.

### Interfaces are consumer-defined seams

In Go the interface belongs in the package that *uses* it, not the package that
implements it. This inverts the habit from nominally-typed languages. Because Go
interfaces are satisfied implicitly, the consumer declares exactly the narrow set
of methods it needs, and any type with those methods — the real adapter, a fake, a
generated mock — satisfies it without an `implements` clause. This is the
interface-segregation principle made concrete: a one-method `Sender`, a two-method
`Repository`. The payoff is direct and mechanical: **a narrow interface yields a
small double.** A one-method port gives you a two-line hand-rolled double; a fat
twelve-method interface forces a fat mock and is itself a design smell. When a
mock is painful to write, the interface is usually too big.

The corollary matters when the collaborator is a third-party client you do not own
(an object-store SDK, a database client). Do not mock its whole fat surface.
Define your *own* narrow interface in your consumer package capturing only the two
or three methods you actually call, adapt the fat client to it (implicitly — the
fat client already satisfies it), and mock that. Exercise 9 drives this directly.

### Dependency injection is the enabler

None of this works if collaborators are reached through package-level globals or
constructed at `init` time with a live network dial. The collaborator must arrive
through the constructor as an interface value:

```go
func NewOrderService(repo OrderRepository, pay PaymentGateway, clk Clock) *Service {
	return &Service{repo: repo, pay: pay, clk: clk}
}
```

Production wires the real adapters; the test passes doubles. No singletons, no
`init()` side effects, no global `http.DefaultClient` reached implicitly. If a
type reaches out to the world on its own, it cannot be isolated, and no amount of
mocking machinery will save it.

### Hand-rolled doubles versus frameworks

A double is just a type that satisfies the interface. For a one- or two-method
port, hand-rolling one is the right call: zero dependencies, completely readable,
and the recording logic is a slice and a mutex. Reach for a framework when the
interface is wide, when you need strict self-verifying expectations, or when call
ordering is the contract and hand-writing the bookkeeping would be error-prone.

- **gomock** (`go.uber.org/mock`, the maintained successor to the archived
  `golang/mock`) generates compile-time-safe mocks from an interface via
  `mockgen`. It does *strict* expectation checking: an unexpected call fails
  immediately, and an unmet expectation fails at cleanup. It has rich matchers
  (`gomock.Any`, `gomock.Eq`, `gomock.Cond[T]`), ordering (`gomock.InOrder`,
  `Call.After`), and dynamic behavior (`DoAndReturn`).
- **testify/mock** is reflection-based and terse. You embed `mock.Mock`, implement
  each method as `args := m.Called(...)` returning `args.Error(0)` and friends,
  program it with `On(...).Return(...)`, and verify with `AssertExpectations(t)`.
  It is *opt-in strict*: only the expectations you explicitly assert are checked,
  and calls are order-independent by default.

Seniors pick per port. A tiny port gets a hand-rolled spy; a wide or
ordering-sensitive one gets gomock; a service with many terse expectations may get
testify. There is no single right framework, only the right tool for this seam.

### gomock lifecycle

`ctrl := gomock.NewController(t)` creates the controller. On modern Go, passing
`*testing.T` auto-registers `ctrl.Finish` via `t.Cleanup`, so you no longer write
a manual `defer ctrl.Finish()` — unmet expectations fail the test automatically
when it (and its subtests) complete. You program expectations through the
generated recorder:

```go
mockRepo.EXPECT().FindByID(ctx, "o-1").Return(order, nil).Times(1)
mockRepo.EXPECT().Save(ctx, gomock.Any()).Return(nil)
```

`Times(n)` / `AnyTimes()` / `MinTimes(n)` bound the call count; `gomock.Any()`
matches any argument, `gomock.Eq(x)` matches by equality, `gomock.Cond(func(x T)
bool)` matches by a predicate. `gomock.InOrder(callA, callB, ...)` and
`callB.After(callA)` pin ordering. `DoAndReturn(func(...))` runs arbitrary logic
and computes the return values dynamically — useful for injecting an error on a
specific argument or capturing what was passed. The strictness cuts both ways: do
not set an expectation you do not intend, because a *missing* call is now a
failure just as an *unexpected* one is.

### testify/mock lifecycle

You write the mock method bodies by hand (or generate them with `mockery`); the
body must funnel through `m.Called` and return the recorded values in the exact
order of the real signature:

```go
func (m *MockInventory) Available(ctx context.Context, sku string) (int, error) {
	args := m.Called(ctx, sku)
	return args.Int(0), args.Error(1)
}
```

Program it with `m.On("Available", ctx, sku).Return(5, nil).Once()`, match
arguments loosely with `mock.Anything` or by predicate with
`mock.MatchedBy(func(...) bool)`, and verify at the end with
`mock.AssertExpectations(t)` (all `On`-expectations were met) or
`AssertNotCalled(t, "Reserve")` (this method was never called). The single most
common bug is mis-indexing: writing `args.Error(0)` when the error is the second
return, so the mock silently hands back a nil error the SUT was supposed to see.
Mirror the real return order exactly.

### Mocking the wall clock

Time is a collaborator like any other. Retry/backoff, TTL expiry, and deadline
logic all consult the clock, and testing them with real `time.Sleep` makes the
suite slow and flaky. The seam is a small `Clock` interface — `Now() time.Time`
plus whatever scheduling primitive the code needs (`Sleep(d)`, or `After(d)
<-chan time.Time`). Production injects a real clock backed by the `time` package;
the test injects a fake clock that records the durations it was asked to sleep and
advances virtual time instantly. A backoff sequence of 100ms, 200ms, 400ms is then
asserted in microseconds with no real waiting. (Go 1.25's `testing/synctest` is an
alternative that virtualizes the real `time` package underneath unmodified code —
covered in its own chapter; the injected-`Clock` pattern here still matters for
code that must build on older toolchains or control time in production.)

### Mocking outbound HTTP without a socket

For pure client-side logic — status-code handling, body decoding, error mapping —
you do not need a real server. `http.RoundTripper` is a single-method seam:
`RoundTrip(*http.Request) (*http.Response, error)`. Inject a fake transport into
`http.Client{Transport: fake}` and have it return canned `*http.Response` values
you construct in the test. This is dramatically cheaper than standing up an
`httptest.Server` and isolates exactly the code you are testing. Two rules keep it
from panicking: always set `Body` to `io.NopCloser(strings.NewReader(...))` (the
client *will* call `resp.Body.Close()`, and a nil or non-closable body panics or
leaks), and always set a valid `StatusCode`. The fake can also capture the outgoing
`*http.Request` so you assert the URL, method, and headers the client built.
`httptest.Server` remains the right tool when you specifically want the *real*
transport exercised end-to-end; `RoundTripper` is right when you want to isolate
client logic from the network entirely.

### Mocking the database boundary: two levels

There are two distinct levels at which you can isolate the database, and they
catch different bugs.

**Level 1 — an in-memory fake of your repository interface.** Fast, real behavior,
trivial to write, and it lets the service layer be tested with pure state
verification. Its blind spot: it never exercises a single line of SQL, so it
cannot catch a typo in a query, a wrong column, or a broken transaction sequence.

**Level 2 — go-sqlmock**, which mocks the `database/sql` *driver* underneath your
real repository. The actual SQL string, the argument binding, the row scanning,
and the `Begin`/`Exec`/`Commit`/`Rollback` sequencing all run for real against a
mock driver. You program the expected statements —
`ExpectBegin`, `ExpectQuery(...).WithArgs(...).WillReturnRows(...)`,
`ExpectExec(...).WillReturnResult(...)`, `ExpectCommit`/`ExpectRollback` — and
assert `ExpectationsWereMet()` at the end so a missing, extra, or mis-ordered
statement fails. Use `sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))`
for exact SQL matching; the default is regex, which can silently match statements
you did not intend. The two levels are complementary: the fake proves the service
logic, sqlmock proves the SQL.

### Concurrency-safe doubles

Any double that a *concurrent* system-under-test touches — a worker pool, an
`errgroup`, a fan-out publisher — is shared mutable state, and its recording slice
must be guarded by a `sync.Mutex`. Append under the lock, and copy the slice out
under the lock in the accessor. Without the guard the test passes on a lucky run
but the `-race` detector correctly flags a data race, and records can be dropped
or corrupted. Every hand-rolled spy in this lesson that a goroutine might touch
locks its recorder, and every test runs under `-race`.

### Fidelity and the mock-reality gap

A double encodes your *assumptions* about the collaborator. If the real payment
gateway can return a `429 Too Many Requests` that your stub never models, your
green mock tests are hiding a live bug: the code has never met that response. This
is the fundamental limit of mocking, and it is why over-mocked suites give false
confidence. Mitigations: model the error and edge responses in your doubles (a 429,
a 500, a decline, a timeout), keep doubles honest by deriving them from a shared
contract, and back the critical seams with integration or contract tests that
exercise the real thing periodically. A mock proves your code handles the responses
you thought of; it can never prove your code handles the ones you did not.

## Common Mistakes

### Over-mocking things you own

Wrong: replacing pure functions, value objects, or in-memory structures you fully
control with mocks, coupling the test to the exact sequence of internal calls. A
later refactor that preserves behavior but changes the call pattern breaks the
test even though nothing a user observes has changed.

Fix: mock only true boundaries — I/O, the clock, external services. For everything
else use the real code and in-memory fakes. If you *can* just run it, run it.

### Asserting interactions instead of outcomes

Wrong: a test that checks only "`repo.Save` was called" and never checks *what*
was saved. It passes while the handler persists a half-populated or unnormalized
record.

Fix: assert on the captured arguments (via a spy) or on the fake's resulting state.
Verify the outcome, not merely that a call happened.

### Mocking a type you do not own by wrapping its whole surface

Wrong: taking a third-party client with a dozen methods and generating a
twelve-method mock so you can substitute it.

Fix: define your own narrow consumer-side interface with only the methods you call,
let the fat client satisfy it implicitly, and mock the narrow seam. The double
stays small because the interface is small.

### Leaking shared double state across tests

Wrong: a package-level mock or fake reused across test functions. One test's
recorded calls or seeded rows pollute the next, producing order-dependent passes
and failures.

Fix: construct a fresh double inside each test function and each subtest. Never
share a mutable double.

### Forgetting the concurrency guard

Wrong: a spy whose `calls` slice is appended from goroutines with no mutex. It
passes on a normal run and fails under `-race`, and can silently drop records.

Fix: lock around the append and around the accessor that copies the slice out. Run
`go test -race`.

### Assuming an unmet gomock expectation is harmless

Wrong: setting `EXPECT()` calls "just in case" and assuming leftover ones do no
harm. With `NewController(t)` the auto-`Finish` fails the test for any *missing*
call, and an *unexpected* call fatals immediately.

Fix: set exactly the expectations you intend. Use `AnyTimes`/`MinTimes`
deliberately when a call is optional, not as a blanket escape hatch.

### Mis-writing a testify mock method body

Wrong: a method body that does not return the `m.Called(...)` values, or that
mis-indexes them (`args.Error(0)` when the error is return position 1). The mock
silently returns zero values and the SUT sees a nil error it should never have
seen.

Fix: every method body funnels through `m.Called` and returns the results in the
exact order of the real signature.

### go-sqlmock regex surprises and skipped verification

Wrong: relying on the default `QueryMatcherRegexp` and writing an `ExpectExec`
pattern loose enough to match an unintended statement, or forgetting
`ExpectationsWereMet()` so a missing or extra query goes unreported.

Fix: use `QueryMatcherOption(QueryMatcherEqual)` for exact SQL, and always assert
`ExpectationsWereMet()` at the end of the test.

### A canned response with a nil or non-closable body

Wrong: building a `*http.Response` in a fake `RoundTripper` with `Body: nil` or a
plain `*strings.Reader`. When the client calls `resp.Body.Close()` it panics or the
type does not satisfy `io.ReadCloser`.

Fix: always set `Body: io.NopCloser(strings.NewReader(...))` and a valid
`StatusCode`.

### Testing time-dependent logic with real sleeps

Wrong: `time.Sleep` inside a retry/backoff unit test to "let the backoff happen."
The suite gets slow and flaky, and the assertion still races the real clock.

Fix: inject a `Clock` and advance a fake clock; assert the recorded durations.
Never sleep for correctness in a unit test.

Next: [01-hand-rolled-mock-notifier.md](01-hand-rolled-mock-notifier.md)
