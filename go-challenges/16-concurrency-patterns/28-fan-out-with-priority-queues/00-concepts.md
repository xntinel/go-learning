# 28. Fan-Out with Priority Queues — Concepts

Fan-out dispatches work from one source to many workers. Putting a priority queue in front of that dispatch changes the guarantee: instead of serving work in arrival order, the system serves it in urgency order. This matters whenever a backlog can accumulate and some items are worth more than others — payment capture versus analytics, a paging alert versus a debug trace, an SLA-bound request versus a nightly batch. This file is the conceptual foundation. Read it once and you will have everything you need to reason through the exercises, which build three independent, self-contained Go modules: a priority dispatcher that fans work out in urgency order, a preemptive job dispatcher where a high-priority job jumps an existing backlog, and a weighted-fair scheduler that serves urgent work first without ever starving the low tier.

## Why a FIFO channel is not enough

A Go channel is a first-in-first-out queue. Every value waits behind the value that was sent before it, regardless of how urgent it is. Under load this is exactly the wrong behaviour. A burst of low-priority work fills the channel buffer, and a high-priority item sent afterwards must wait for the entire backlog to drain before a worker ever sees it. The tail latency of the urgent item becomes a function of how much unimportant work happened to arrive first.

A priority queue breaks that coupling. It is a data structure whose "next" element is always the most urgent one currently held, not the oldest one. Reordering happens on every insertion, so a high-priority item that arrives last can still come out first. The cost is that a channel's send and receive are O(1) and lock-free on the fast path, whereas a priority queue insert and extract are O(log n) and need a place to live that many goroutines can reach safely. The rest of this lesson is about paying that cost well.

## The heap contract

Go's standard library gives you the algorithm but not the container. `container/heap` implements the sift-up and sift-down operations of a binary heap against any type that satisfies `heap.Interface`: the three methods of `sort.Interface` (`Len`, `Less`, `Swap`) plus `Push` and `Pop`. You supply the storage — almost always a slice — and the five methods; the package supplies `heap.Init`, `heap.Push`, `heap.Pop`, `heap.Fix`, and `heap.Remove`, each maintaining the heap invariant in O(log n).

`Less` is the only place priority lives. For a min-heap, where the smallest value is the most urgent, `Less(i, j)` returns true when element `i` should be served before element `j`. Encoding "lower number means more urgent" as `Less` returning `h[i].priority < h[j].priority` makes priority 0 the top of the queue. A secondary comparison on an enqueue sequence number or timestamp — `return h[i].seq < h[j].seq` when priorities tie — gives first-in-first-out behaviour among items of equal priority, which is what keeps a steady stream of equal-priority work from reordering itself arbitrarily.

There is one bookkeeping subtlety that bites everyone once. To support `heap.Fix` and `heap.Remove`, each element usually stores its own current index into the backing slice, and `Swap` must update those index fields whenever it exchanges two elements. Forget it and `Fix`/`Remove` will operate on the wrong element, silently corrupting the heap. `Push` and `Pop` operate on the pointer receiver (`*T`) because they change the slice length; `Len`, `Less`, and `Swap` take the value receiver because they only read or permute. A classic mistake is implementing `Pop` to return the *first* element — the heap convention is that `Pop` removes the *last* slice element (the package has already moved the minimum there for you), so `Pop` is a constant-time slice trim, not a search.

## Two ways to make the heap concurrency-safe

A heap is not safe for concurrent use: `Push` and `Pop` both read and write the backing slice and call `Swap`. Two patterns make it safe for a fan-out system, and the exercises use both deliberately.

The first is goroutine confinement. One goroutine — the dispatcher loop — owns the heap and is the only code that ever touches it. Producers send items to it over a channel; it inserts them, extracts the most urgent, and sends that to a `work` channel that the worker pool reads. No lock guards the heap because only one goroutine reaches it. This keeps the ordering logic in one readable place and avoids lock contention on the hot path, at the cost of one extra hop (producer to loop to worker). It is the cleanest design when the queue is a pipeline stage.

The second is a mutex with a condition variable. The heap is wrapped in a struct with a `sync.Mutex`; `Enqueue` locks, pushes, signals, and unlocks, while each worker calls a blocking `Dequeue` that waits on a `sync.Cond` until the heap is non-empty, then pops the minimum under the lock. This turns the priority queue into a shared object that any number of workers can pull from directly, with no dispatcher goroutine in the middle. It is the natural shape for a job dispatcher where workers are long-lived and you want them blocking directly on "give me the most urgent job." A `sync.Cond` is the right tool here precisely because the wait condition ("the heap is non-empty") is not a simple value a channel can carry; `cond.Wait` atomically releases the lock and re-acquires it on wake, and the wait must sit in a `for` loop because `Signal` permits spurious-looking wakeups and because another worker may empty the queue between the signal and the wake.

Which to reach for is a real design decision, not a style preference. Confinement composes well into channel pipelines and makes cancellation a matter of selecting on a context. A `Cond`-guarded queue lets workers block directly on the data structure and makes "enqueue is synchronous — once `Enqueue` returns, the item is provably in the queue" easy to guarantee, which in turn makes ordering tests deterministic.

## Drain before dispatch, and never lose the popped item

When the dispatcher-loop design has many items already buffered on its incoming channel, it should drain all of them into the heap before extracting the next item to dispatch. Dispatching after a partial drain means the heap had an incomplete view of the workload and may hand out an item that is not actually the most urgent one available. The shape is: if the heap is empty, block on the incoming channel rather than spinning; otherwise do a non-blocking drain of everything currently queued, then pop the minimum and try to send it.

The send step needs care. If the `work` channel is full because all workers are busy, the loop must not drop the item it just popped. The correct move is to `select` over three cases at once: sending the popped item to a worker, receiving a newly arrived item from incoming, or context cancellation. If a new item wins the race, the loop pushes *both* the popped item and the new one back into the heap and loops again — the popped item is preserved, and on the next iteration the heap re-decides which of the two is more urgent. The detail that is easy to get wrong: the enqueue timestamp or sequence number must be assigned once, at submit time, and never overwritten when an item is requeued. Stamp it on the requeue and the item looks newer than work that genuinely arrived later, corrupting the first-in-first-out tiebreak within its tier.

## What ordering you can actually promise

This is the honesty section, and it is where most priority fan-out systems are described incorrectly. A priority queue guarantees the order in which items are *extracted* from the queue — the dispatch order. It does not, on its own, guarantee the order in which N concurrent workers *finish* them — the completion order. With more than one worker, a slightly-less-urgent job handed to a fast worker can finish before a more-urgent job handed to a slow one. Any test that submits work to a multi-worker pool and asserts a specific completion order is testing timing, not priority, and it will flake.

So the assertions that are both meaningful and deterministic are: with a single worker draining a fully-populated queue, the extraction order is exactly priority order (ties broken first-in-first-out); and a high-priority item enqueued while a low-priority backlog is already waiting is the very next item extracted — it preempts the queue. To make either assertion reliable you must remove the race between "producer is still enqueuing" and "worker is already consuming." Two techniques do this: enqueue the whole batch before any worker starts, or gate the worker between jobs so the test controls exactly when the next extraction happens. The exercises use both, and they assert dispatch order, never multi-worker completion order. Under genuine load you can still assert aggregate properties — every submitted job is eventually processed, no job is lost, the run is race-clean, and the average dispatch position of the high tier is earlier than that of the low tier — without pretending a total completion order exists.

## Preemption versus interruption

"A high-priority job preempts the queue" has a precise and limited meaning here: when the dispatcher next chooses an item, it chooses the newly-arrived high-priority one ahead of lower-priority items already waiting. It jumps the *queue*. It does not interrupt a job already running on a worker. Cooperative preemption of in-flight work — pausing a running job to run a more urgent one — requires the job itself to check a context or yield at checkpoints; the scheduler cannot forcibly suspend a goroutine mid-computation. Conflating the two leads to tests that try to assert a running low job was "stopped," which the design neither does nor should. The realistic guarantee is queue preemption, and that is exactly what is testable: hold a worker on a job, enqueue an urgent item behind it, release the worker, and the urgent item — not the next low item — is what it picks up.

## Starvation, and why strict priority is not enough

A strict priority queue has a fatal property under sustained load: it starves the low tier. As long as high-priority work keeps arriving, low-priority work is never the minimum and is never served. For a logging pipeline that drops debug traces under pressure this is acceptable, even desirable. For anything where the low tier must still make progress — background reconciliation that must eventually run, a free-tier customer who must still get *some* service — it is a liveness bug.

Three families of fix exist. Aging gradually raises a waiting item's effective priority as a function of how long it has waited, so a long-delayed low item eventually outranks fresh high items; `heap.Fix` is the operation that re-sorts an item after its key changes. Rate limiting caps how much of each tier is admitted, so the high tier cannot monopolise the server. And weighted fair queuing apportions service capacity among tiers by weight rather than by absolute precedence, so a 9:1 weighting means the high tier gets roughly nine units of service for every one the low tier gets — but the low tier always gets that one, and is never fully starved.

Weighted fair queuing is worth understanding concretely because it is what real schedulers use. The cleanest formulation assigns each arriving job a *virtual finish time*: `vfinish = max(virtualClock, lastFinish[tier]) + cost / weight[tier]`. The scheduler always serves the job with the smallest virtual finish time and advances its virtual clock to that value. A heavier weight divides the cost by a larger number, so a high-weight tier accrues virtual time slowly and is chosen more often; but a starved low tier's `lastFinish` falls behind the virtual clock, so the `max` pins its next virtual finish time near the present and it is guaranteed to be chosen again within a bounded number of steps. The result is provably fair: over any window, each tier receives service in proportion to its weight, and the gap between two consecutive low-tier services is bounded by the weight ratio rather than unbounded as under strict priority. Deficit round robin reaches the same goal with integer counters instead of virtual time and is what Linux's network queueing disciplines use; the underlying idea — give each tier a quantum proportional to its weight and carry forward the unused remainder — is the same.

The practical lesson is that "priority" and "fairness" are different requirements that need different structures. Reach for a strict heap when starvation of the low tier is acceptable. Reach for weighted fair queuing the moment "the low tier must always make progress" becomes a requirement, and make the no-starvation property a thing your tests actually assert: bound the maximum gap between low-tier services and verify it holds even while the high tier always has work.

## Graceful shutdown

A dispatcher owns goroutines, so it owns their lifecycle. The portable shutdown primitive is context cancellation: every loop and every worker selects on `ctx.Done()` and returns when it fires, and a `done` channel (or a `sync.WaitGroup`) lets the caller block until everything has actually stopped rather than racing ahead. The design decision is what to do with work still in the heap or in flight at shutdown. The simplest correct behaviour, and the right default for most systems, is to cancel and discard: in-flight jobs see a cancelled context and abandon, queued jobs are dropped. If losing queued work is unacceptable, drain the heap to durable storage before returning from `Stop`, which turns shutdown into a small flush protocol. Either way, `Stop` must be idempotent and must not deadlock if called twice or called concurrently with a producer — a `sync.Once` around the cancel plus a flag checked under the lock on the submit path is the usual shape.

## Common Mistakes

### Forgetting to update the index field in Swap

If heap elements carry their own index for `heap.Fix`/`heap.Remove`, `Swap` must write the new indices after exchanging the elements. Omit it and `Fix` re-sorts the wrong element, which corrupts the heap silently — items come out in the wrong order or a stale index drives a nil dereference. The fix is one line per swapped element: after `h[i], h[j] = h[j], h[i]`, set `h[i].index = i` and `h[j].index = j`.

### Asserting multi-worker completion order

A test that fans work out to several workers and then asserts the results arrive in priority order is asserting timing. A faster worker can finish a less urgent job before a slower worker finishes a more urgent one. Such a test passes on a quiet laptop and flakes in CI. Assert dispatch order with a single worker or a gate, or assert aggregate properties under load (every job processed, race-clean, high tier dispatched earlier on average) — never a total completion order across concurrent workers.

### Overwriting the enqueue timestamp on requeue

When the loop pushes a popped item back because the work channel was momentarily full, it must not re-stamp the item's enqueue time or sequence number. Re-stamping makes a requeued item look newer than work that genuinely arrived after it, breaking the first-in-first-out tiebreak within a priority tier. Stamp once at submit time, guarded by an "if zero" check, and treat the stamp as immutable thereafter.

### Spinning on an empty queue

Looping `if pq.Len() > 0 { pop; dispatch }` with no blocking case burns a whole CPU core doing nothing whenever the queue is empty. When the heap is empty, block — on the incoming channel in the confinement design, or on a `sync.Cond` in the shared-queue design — so the goroutine parks until there is work. The `Cond` wait must sit inside a `for !condition` loop, never a single `if`, because the queue can be emptied by another worker between the signal and the wake.

### Mistaking a strict priority heap for a fair scheduler

A plain min-heap on priority starves the low tier under sustained high-priority load, by construction. If a requirement says the low tier must always make progress, a strict heap does not meet it no matter how the tests are written; you need aging, rate limits, or weighted fair queuing, and you need a test that asserts the bounded-gap no-starvation property rather than only asserting that the high tier is served first.

---

Next: [01-priority-dispatcher.md](01-priority-dispatcher.md)
</content>
</invoke>
