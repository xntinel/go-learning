# 27. Building a Concurrent Web Crawler — Concepts

A concurrent web crawler is the canonical exercise for combining a bounded worker pool, deduplication, dynamic work discovery, depth limiting, politeness, and a clean shutdown with no leaked goroutines. The reason it is canonical is that every one of those concerns is a separate, named concurrency problem, and a crawler forces all of them to be correct at once: workers discover new work while other workers are still running, the same URL surfaces from many pages simultaneously, the graph can be larger than memory, and the program must decide — without a fixed count known up front — when there is no more work and it is safe to stop. This file is the conceptual foundation for the three modules that follow: read it once and you will have the model needed to reason through the core crawler, a politeness-aware bounded crawler, and a concurrent link checker, each built as an independent, self-contained Go module over an injected fake fetcher so the tests never touch the real network.

## Concepts

### The coordination problem: check-and-mark must be atomic

A sequential crawler is trivial: fetch a page, collect its links, push them onto a stack, repeat, skipping any URL already seen. A concurrent crawler loses that simplicity because multiple workers discover new URLs simultaneously. Worker A and worker B can both pull a page that links to `/x` at the same instant; if each independently checks a `seen` set, finds `/x` absent, and then enqueues it, `/x` is fetched twice. The fix is to make the check and the mark a single indivisible step: acquire a mutex, test `seen[url]`, and — only if absent — set `seen[url] = true` before releasing the lock. Splitting the test and the set into two critical sections reintroduces the race. The coordination itself is cheap; what matters is that the lock spans the whole read-modify-write, not just one half of it.

A second, quieter consequence is that the deduplication set is the authority on "have we committed to this URL," so it must be updated at the moment of enqueue, not at the moment of fetch. If you mark a URL seen only when a worker starts fetching it, two workers can both enqueue it before either fetches, and the mark comes too late. Mark on enqueue, fetch later.

### Dynamic work and termination detection

Static work is easy: a fixed slice of N items, a `WaitGroup` initialized to N, each worker calls `Done`, main calls `Wait`. Dynamic work is the hard case, because workers add items to the queue while other workers are still processing, so the total count is unknown until the crawl is already finished. The standard solution is a counting `WaitGroup` that tracks *items enqueued but not yet fully processed*, not the number of workers:

1. Increment the counter just before an item is placed on the queue.
2. Decrement it only after the item is fully processed — meaning after every child URL it discovered has itself been enqueued.
3. A dedicated closer goroutine calls `Wait` on that counter and, when it reaches zero, closes the queue channel.
4. Workers `range` over the queue and exit naturally when it is closed.

The ordering in steps 1 and 2 is the entire game. The increment must happen *before* the matching item can possibly be marked done, and the decrement must happen *after* all of that item's children are enqueued (each of which performed its own increment first). If those two rules hold, the counter is zero if and only if there is genuinely no outstanding work, and the closer can shut the queue safely. If either rule is violated, the counter can momentarily read zero while work is still in flight, and the closer shuts the queue early, truncating the crawl.

Tracking items rather than workers is what makes the count meaningful: workers are a fixed pool that lives for the whole crawl, so counting them tells you nothing about whether work remains. The unit of work is the URL, so the URL is what the counter must follow.

### The two ordering constraints, stated as code

The increment-before-enqueue rule, in the body of an `add` helper that the workers and the seeding step both call:

```text
mu.Lock()
if seen[url] || depth > maxDepth || count >= maxPages { mu.Unlock(); return }
seen[url] = true
count++
inFlight.Add(1)   // BEFORE Unlock: the closer cannot observe zero before the item lands
mu.Unlock()
queue <- task{url, depth}
```

The done-after-children rule, in the worker loop:

```text
for t := range queue {
	links, err := fetcher.Fetch(t.url)
	record(t, err)
	for _, link := range links {
		add(link, t.depth+1)   // every child increments inFlight first
	}
	inFlight.Done()            // only now is THIS item complete
}
```

If `inFlight.Add(1)` is moved after `mu.Unlock()`, the window between the unlock and the add lets the closer goroutine observe a zero counter and close the queue before the new item is enqueued. If `inFlight.Done()` is moved before the `add` loop, the counter can drop to zero while children are still being enqueued, and again the queue closes early. Both bugs are invisible in a small, lucky run and surface as a flaky, short crawl under load — exactly the kind of defect the race detector and a stress test are meant to expose.

### Queue capacity and backpressure

A worker that enqueues a child performs a channel send. If the queue channel is unbuffered or its buffer is full, that send blocks until another worker receives. When the discovered frontier is wider than the number of receivers and every worker is simultaneously blocked trying to send, no worker is left to receive, and the program deadlocks. There are two standard ways out, and they trade different costs.

The first is to give the queue a buffer large enough that the in-flight frontier never fills it in practice. This is simple and adds no goroutines, and it is correct for graphs whose frontier width stays under the buffer size; its failure mode is a hang on a graph wider than the buffer. The core module below takes this approach with a generous buffer and is explicit that it is bounded by that buffer.

The second is to perform each enqueue from its own short-lived goroutine, `go func() { queue <- task }()`, so the sending worker never blocks. This removes the deadlock entirely at the cost of spawning a goroutine per edge, which is cheap but unbounded. A third, more disciplined option — used by the politeness module — is to keep an internal unbounded work list in the coordinator and hand items to a fixed worker pool through a rendezvous, so neither the buffer size nor the goroutine count grows with the graph.

### Bounded parallelism: why a fixed worker pool

Spawning one goroutine per URL is tempting and wrong for anything real. Goroutines are cheap but not free, and the resource that actually matters is downstream: open sockets, file descriptors, memory for in-flight response bodies, and the politeness budget of the servers being crawled. An unbounded crawler will happily open ten thousand simultaneous connections and either exhaust local file descriptors or get the crawler's IP throttled or banned. A fixed pool of N worker goroutines, each pulling the next URL from a shared queue, caps concurrency at exactly N regardless of how large the graph is. N becomes a single knob that bounds memory, sockets, and outbound request rate together. This is the difference between a toy and a crawler you can point at a real site.

### Depth limiting

Most crawls are scoped: fetch the seed, the pages it links to, the pages those link to, down to some maximum depth, then stop. Depth is carried alongside each URL as part of the work item. The seed is depth 0; a link found on a depth-`d` page is enqueued at depth `d+1`; the enqueue step refuses any URL whose depth would exceed the configured maximum. The refusal must happen at enqueue time, inside the same critical section as the dedup check, so that an over-deep URL is never even marked seen — otherwise the same URL arriving later at a shallower depth (via a different path) would be wrongly skipped as already-visited. The invariant to test is twofold: no recorded result has a depth greater than the maximum, and a page that sits one level below the limit is never fetched at all.

### Politeness: per-host rate limiting

A correct crawler is not the same as a polite one. Hammering a single host with hundreds of concurrent requests is hostile regardless of how clean the concurrency is, and real servers respond by throttling, returning 429, or blocking the source. Politeness means rate-limiting per host: between two requests to the same host, wait at least some minimum interval, and typically allow only one in-flight request per host at a time. The standard design keys a small table by host (parsed from the URL's authority), and guards each host with its own state — a mutex plus the timestamp of that host's last fetch. A worker about to fetch a URL acquires that host's lock, sleeps for whatever remains of the interval since the host was last touched, records the new timestamp, performs the fetch, and only then releases the host lock. Holding the host lock across the fetch gives two properties at once: requests to the same host are serialized (never overlapping), and consecutive requests to that host are spaced by at least the interval. Crucially, this is per host, so a pool of N workers still runs fully in parallel across N *different* hosts; only same-host traffic is paced. The politeness layer composes cleanly with the fetcher by wrapping it: the rate-limited fetcher is itself a `Fetcher`, so the crawler core is unchanged.

### The Fetcher interface: dependency injection for testability

Production code makes HTTP calls. Tests must not. The seam that resolves this tension is a one-method interface — `Fetch(url string) (links []string, err error)` — that the crawler depends on instead of depending on `net/http` directly. The real implementation wraps an `*http.Client` and parses anchors out of the response; the test implementation is a static in-memory graph, a `map[string][]string`, that implements the same method and returns deterministic results with zero network access. A function-adapter type lets any plain function satisfy the interface without a named struct, which is convenient for a test that needs to count calls or inject a specific error. This injection is not a testing convenience bolted on after the fact; it is the design decision that keeps the crawler's core package free of network dependencies and makes every concurrency invariant — each URL fetched exactly once, depth respected, no leaks — checkable as a fast, deterministic unit test under the race detector.

### Link checking: bounded fan-out over a result set

A link checker is a crawler's simpler cousin and a common real task: given a set of URLs, report which are broken. "Broken" means the request errored or the server answered with a status at or above 400. There is no recursion and no dynamic work discovery — the set of URLs is known up front — so the problem reduces to bounded fan-out: distribute a fixed list of independent checks across a fixed pool of workers, collect the results, and report the broken ones. The two things that make it non-trivial are the same two that make any fan-out non-trivial: bounding parallelism so the checker does not open a connection per link, and collecting results without a data race (either through a results channel drained by one collector, or a shared slice appended under a mutex and read only after every worker has finished). Because the work set is static, termination is trivial — close the input channel once all URLs are queued, and workers drain and exit — which makes the link checker the right place to see the fan-out skeleton in isolation, without the termination-detection machinery the recursive crawler needs.

### Goroutine leaks and clean shutdown

A concurrency bug that no functional test catches is the leak: a crawl returns the right answer but leaves a worker or a closer goroutine blocked forever on a channel that will never receive or close. Leaks accumulate across many crawls until the process is starved. The discipline that prevents them is to join every goroutine you start before the top-level call returns: the worker pool is awaited with its own `WaitGroup`, and the closer goroutine is awaited too, so that when `Crawl` returns there is provably nothing of its own still running. The test for this is to record the live goroutine count before a batch of crawls and confirm it returns to that baseline afterward — a property that holds only if every goroutine has a guaranteed exit path and is actually awaited.

## Common Mistakes

### Splitting the dedup check and mark into two critical sections

Wrong: lock, read `seen[url]`, unlock; then later lock, set `seen[url] = true`, unlock. Between the two sections another worker runs the same read, also sees the URL absent, and both enqueue it. The URL is fetched twice and may appear twice in the results. Fix: hold one lock across the test and the set, and enqueue inside or immediately after that same critical section.

### Incrementing the in-flight counter after releasing the lock

Wrong: mark seen and unlock, then call `inFlight.Add(1)` and send. In the gap between the unlock and the add, the closer goroutine can observe a zero counter (the previous item's `Done` having just landed) and close the queue, so the send panics on a closed channel or the item is lost. Fix: call `inFlight.Add(1)` while the lock is still held, before the unlock, so the counter reflects the new item before any other goroutine can act on its absence.

### Calling Done before all children are enqueued

Wrong: fetch a page, immediately `inFlight.Done()`, then loop enqueuing its children. The `Done` can bring the counter to zero before the children's `Add` calls run; the closer sees zero and closes the queue, and the children are never crawled. Fix: enqueue every child first, then `Done`, so a parent is never reported complete while its descendants are still being discovered.

### Spawning one goroutine per URL instead of using a bounded pool

Wrong: `go fetch(url)` for every discovered link. On a real site this opens thousands of simultaneous connections, exhausts file descriptors, and gets the crawler throttled or banned. Fix: a fixed pool of N workers pulling from a shared queue, so N bounds sockets, memory, and request rate at once.

### Rate-limiting globally instead of per host

Wrong: a single shared limiter that paces all requests regardless of destination. It throttles a crawl that spans many hosts far below its safe throughput, and on a single-host crawl it is no more correct than a per-host limiter while being slower everywhere else. Fix: key the limiter by host so same-host traffic is paced and cross-host traffic runs fully in parallel.

### Leaving a goroutine blocked after the crawl returns

Wrong: start a closer goroutine that does `inFlight.Wait(); close(queue)` but never await it, or start workers and return before they have drained. The crawl returns the right results while a goroutine sits blocked forever. Fix: await the worker pool and the closer with `WaitGroup`s before returning, and verify with a goroutine-count test that the baseline is restored.

---

Next: [01-concurrent-crawler-core.md](01-concurrent-crawler-core.md)
</content>
</invoke>
