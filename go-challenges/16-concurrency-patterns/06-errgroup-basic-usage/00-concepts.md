# 6. errgroup Basic Usage — Concepts

When you fan a piece of work out across N goroutines, three questions decide whether the result is correct: how do you wait for all of them, how do you surface a failure from any one of them, and how do you stop the survivors once one has failed. `golang.org/x/sync/errgroup` answers all three in a dozen lines of public API, and it is the package the Go team itself reaches for. This file is the conceptual foundation for the lesson: read it once and you will understand both how to use `errgroup` and exactly what it does internally, so that each exercise — using the library, rebuilding its core from `sync` primitives, assembling a dashboard by scatter-gather, and validating many fields in parallel — reads as an application of the same small set of ideas rather than a new trick.

## Concepts

### The Problem errgroup Solves

`sync.WaitGroup` waits for goroutines to finish, and that is all it does. It has no notion of an error: if one of the goroutines fails, the WaitGroup neither knows nor cares, and the caller is left to invent a side channel to carry the failure out. The naive side channel is a shared `error` variable guarded by a `sync.Mutex`, with every goroutine doing `mu.Lock(); err = ...; mu.Unlock()`. That design has two defects that are easy to miss until they bite. The mutex is sprinkled across every call site, so it is easy to forget one and introduce a data race. And the meaning of "the error" is unclear: a slow goroutine can overwrite a fast one's error, so the value you read out is not reliably the first failure, just the last writer to win the lock.

The pattern that fixes both defects is the one `errgroup` packages: a `WaitGroup` to count the goroutines, a `sync.Once` to capture exactly one error (the first), and a derived `context.Context` whose cancellation tells the other goroutines to stop. The first goroutine to fail records its error under the `Once` and cancels the context in the same critical section; every later failure finds the `Once` already spent and is dropped; every still-running goroutine sees the cancellation and can return early. `Wait` blocks on the `WaitGroup` and then returns the single captured error.

### The Public API

The whole surface is small enough to hold in your head. A zero-value `errgroup.Group` is ready to use and gives you waiting plus first-error collection with no cancellation. The more common constructor is `errgroup.WithContext(parent)`, which returns a `*Group` and a derived `context.Context`. You pass functions to the group with `g.Go(func() error)`; each runs in its own goroutine. You call `g.Wait() error` once, which blocks until every function passed to `Go` has returned and then yields the first non-nil error, or `nil` if all succeeded.

Two contracts about the derived context matter and are worth stating precisely, because exercises depend on them. First, the context is cancelled the moment the first function passed to `Go` returns a non-nil error — that is the signal that lets siblings stop early. Second, the context is also cancelled when `Wait` returns, even on a fully successful run; this guarantees the context never leaks, so a goroutine blocked on `ctx.Done()` is always released. The consequence is concrete: after `g.Wait()` returns, `ctx.Err()` is `context.Canceled` whether the run succeeded or failed.

Two more methods round out the package. `g.SetLimit(n)` caps the number of goroutines that may be active at once; a `g.Go` call blocks until a slot is free, which turns the group into a bounded worker pool in one line. `g.TryGo(func() error) bool` is the non-blocking sibling: it starts the function only if a slot is free and reports whether it did. `SetLimit` must be called before any `Go`, and a negative limit means unbounded.

### Cancellation Is Cooperative, Not Forced

The single most common misunderstanding is to expect `errgroup` to kill a running goroutine when a sibling fails. Go has no mechanism to forcibly stop a goroutine, and `errgroup` does not pretend otherwise. What cancellation does is close the derived context's `Done` channel. A goroutine only stops early if it is written to watch that channel — through a `select` on `<-ctx.Done()`, or by passing `ctx` down into the blocking calls it makes (an `http.Request` built `WithContext(ctx)`, a database query that takes a context, a `time.After` raced against `ctx.Done()`). Work that ignores the context runs to completion regardless of how early a sibling failed. "Fail fast" is therefore a property of the task functions, not a magic of the group: the group provides the cancellation signal, and the tasks must honor it.

### First Error Wins, and "First" Means First to Return

`Wait` returns "an error," and it is specifically the error from whichever `Go` function was the first to return a non-nil value — first in wall-clock completion order, not first in the order you called `Go`. If two functions fail, the one that returns sooner is the one you see; the other is discarded. This has a direct testing consequence: a test that asserts `g.Wait()` equals one specific sentinel is flaky, because on a different machine or under `-race` the completion order can change. The honest assertion is membership — the returned error `errors.Is` one of the errors the functions could produce — unless your task delays make one failure provably first.

### Collecting Every Error Instead of the First

`errgroup`'s first-error-wins model is exactly wrong for one whole class of problems: validating many independent fields, where the user wants to see every problem at once, not be told about them one at a time across repeated submissions. The fix is not a different library; it is to use `errgroup` purely as a goroutine manager and decline its error collection. Each task returns `nil` to the group — so the group never aborts and every task runs — and writes its own result (an `error` or `nil`) into its own pre-assigned slot of a results slice. After `Wait`, the caller folds the slice into a single value with `errors.Join`, which skips the `nil` entries, returns `nil` when every entry is `nil`, and otherwise produces a combined error whose `Unwrap() []error` lets `errors.Is` find each underlying failure. This indexed-slot technique — each goroutine owns one index, written before it returns and read only after `Wait` — is also why no mutex is needed: distinct indices are distinct memory, and the `WaitGroup`'s happens-before edge makes every write visible to the post-`Wait` read.

### Bounded Fan-Out and the Indexed-Result Pattern

Two recurring shapes appear in the exercises and in real services. The first is the bounded scatter-gather: fan a request out to N back-ends, cap the concurrency with `SetLimit` so a burst does not open N thousand sockets at once, and assemble the responses in request order. Because each response lands in `results[i]` for the `i` it was launched with, the assembled slice is already ordered correctly with no sorting step — the order is structural, not something you reconstruct afterward. The second is the all-errors collection above. Both rest on the same discipline: pre-size the result slice, give each goroutine exactly one index, never share an index, and read the whole slice only after `Wait`. Get that discipline right and the `-race` detector stays silent; get it wrong — two goroutines appending to one slice, or a read before `Wait` — and it will tell you immediately, which is why every exercise here is gated under `-race`.

## Common Mistakes

### Expecting Cancellation to Stop Work That Ignores the Context

Wrong: calling `errgroup.WithContext`, passing slow tasks to `Go`, and assuming a sibling's failure will abort them. If a task does not select on `ctx.Done()` or thread `ctx` into its blocking calls, it runs to completion no matter when a sibling fails, and "fail fast" silently becomes "wait for the slowest." Fix: thread the derived context into every blocking operation, and in a hand-rolled select always include a `case <-ctx.Done(): return ctx.Err()` arm.

### Sharing the First Error Through a Mutex Instead of a Once

Wrong: a shared `var err error` plus a `sync.Mutex`, each goroutine locking to overwrite it. The "first error" is then whichever writer last won the lock, not the first to fail, and a forgotten lock is a data race. Fix: a `sync.Once` captures the first error and triggers cancellation in one critical section; later failures find the `Once` spent and are dropped — which is precisely what `errgroup` does internally.

### Asserting a Specific Error Comes Back First

Wrong: a test that pins `g.Wait()` to one sentinel when several tasks can fail. The order of return is the order of completion, which is not stable across machines or under `-race`. Fix: assert membership with `errors.Is`, or arrange task delays so one failure is provably first before asserting on it.

### Using errgroup's First-Error Model to Collect All Errors

Wrong: trying to gather every validation failure by returning each from `Go` and reading `Wait`'s single result — you get one error and the rest vanish, and the failing tasks may have cancelled the rest before they ran. Fix: return `nil` from every `Go` task, record each task's outcome in its own results slot, and combine them with `errors.Join` after `Wait`.

### Sharing One Slice Index Across Goroutines

Wrong: multiple goroutines appending to a shared slice, or writing the same index, to gather results. That is a data race and `-race` will flag it. Fix: pre-size the slice to the number of tasks, hand each goroutine the single index it was launched with, write that index before returning, and read the slice only after `Wait`.

---

Next: [01-errgroup-basics.md](01-errgroup-basics.md)
