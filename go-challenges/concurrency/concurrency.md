# Concurrency

Go's concurrency model -- built on goroutines, channels, and the select statement -- is one of the language's defining features. These 123 exercises take you from spawning your first goroutine through production-grade patterns like pipelines, rate limiters, and graceful shutdown.

> 123 exercises | 9 sections | basic to advanced

---

## 01 - Goroutines and Scheduling

Understand how Go's runtime manages lightweight threads: the GMP model, stack growth, GOMAXPROCS, and cooperative scheduling.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Launching Goroutines](01-goroutines-and-scheduling/01-launching-goroutines/01-launching-goroutines.md) | basic | 15m |
| 2 | [Goroutine vs OS Thread](01-goroutines-and-scheduling/02-goroutine-vs-os-thread/02-goroutine-vs-os-thread.md) | basic | 20m |
| 3 | [GMP Model in Action](01-goroutines-and-scheduling/03-gmp-model-in-action/03-gmp-model-in-action.md) | intermediate | 30m |
| 4 | [Goroutine Stack Growth](01-goroutines-and-scheduling/04-goroutine-stack-growth/04-goroutine-stack-growth.md) | intermediate | 30m |
| 5 | [GOMAXPROCS and Parallelism](01-goroutines-and-scheduling/05-gomaxprocs-and-parallelism/05-gomaxprocs-and-parallelism.md) | intermediate | 30m |
| 6 | [Cooperative Scheduling](01-goroutines-and-scheduling/06-cooperative-scheduling/06-cooperative-scheduling.md) | intermediate | 30m |
| 7 | [Goroutine Per Request](01-goroutines-and-scheduling/07-goroutine-per-request/07-goroutine-per-request.md) | intermediate | 25m |
| 8 | [A Million Goroutines](01-goroutines-and-scheduling/08-million-goroutines/08-million-goroutines.md) | advanced | 45m |
| 9 | [Goroutine Lifecycle](01-goroutines-and-scheduling/09-goroutine-lifecycle/09-goroutine-lifecycle.md) | intermediate | 30m |
| 10 | [Goroutine Leak Detection](01-goroutines-and-scheduling/10-goroutine-leak-detection/10-goroutine-leak-detection.md) | intermediate | 35m |
| 11 | [Goroutine Error Handling](01-goroutines-and-scheduling/11-goroutine-error-handling/11-goroutine-error-handling.md) | intermediate | 30m |
| 12 | [Goroutine Return Values](01-goroutines-and-scheduling/12-goroutine-return-values/12-goroutine-return-values.md) | intermediate | 30m |
| 13 | [Concurrent API Calls](01-goroutines-and-scheduling/13-concurrent-api-calls/13-concurrent-api-calls.md) | intermediate | 30m |
| 14 | [Background Job Processor](01-goroutines-and-scheduling/14-background-job-processor/14-background-job-processor.md) | intermediate | 35m |
| 15 | [Concurrent File Processing](01-goroutines-and-scheduling/15-concurrent-file-processing/15-concurrent-file-processing.md) | intermediate | 30m |
| 16 | [Goroutine Supervision](01-goroutines-and-scheduling/16-goroutine-supervision/16-goroutine-supervision.md) | advanced | 40m |
| 17 | [Concurrent Map-Reduce](01-goroutines-and-scheduling/17-concurrent-map-reduce/17-concurrent-map-reduce.md) | advanced | 40m |
| 18 | [Connection Pool](01-goroutines-and-scheduling/18-connection-pool/18-connection-pool.md) | advanced | 45m |
| 19 | [Parallel Validation](01-goroutines-and-scheduling/19-parallel-validation/19-parallel-validation.md) | intermediate | 30m |
| 20 | [Goroutine-Safe Cache with TTL](01-goroutines-and-scheduling/20-goroutine-safe-cache/20-goroutine-safe-cache.md) | advanced | 45m |
| 21 | [Goroutine Profiling with pprof](01-goroutines-and-scheduling/21-goroutine-profiling-pprof/21-goroutine-profiling-pprof.md) | advanced | 45m |
| 22 | [Testing Concurrent Code](01-goroutines-and-scheduling/22-testing-concurrent-code/22-testing-concurrent-code.md) | advanced | 45m |
| 23 | [Dynamic Goroutine Scaling](01-goroutines-and-scheduling/23-dynamic-goroutine-scaling/23-dynamic-goroutine-scaling.md) | advanced | 50m |
| 24 | [Goroutine-Per-Connection TCP Server](01-goroutines-and-scheduling/24-goroutine-per-connection-tcp/24-goroutine-per-connection-tcp.md) | advanced | 50m |
| 25 | [Goroutine Coordination Barrier](01-goroutines-and-scheduling/25-goroutine-coordination-barrier/25-goroutine-coordination-barrier.md) | intermediate | 35m |
| 26 | [Goroutine-Safe Service Registry](01-goroutines-and-scheduling/26-goroutine-safe-service-registry/26-goroutine-safe-service-registry.md) | advanced | 45m |
| 27 | [Scatter-Gather with Partial Results](01-goroutines-and-scheduling/27-scatter-gather-partial-results/27-scatter-gather-partial-results.md) | intermediate | 35m |
| 28 | [Goroutine Starvation and Fairness](01-goroutines-and-scheduling/28-goroutine-starvation-fairness/28-goroutine-starvation-fairness.md) | advanced | 45m |
| 29 | [Goroutine Work Stealing](01-goroutines-and-scheduling/29-goroutine-work-stealing/29-goroutine-work-stealing.md) | advanced | 50m |
| 30 | [Goroutine DAG Orchestrator](01-goroutines-and-scheduling/30-goroutine-dag-orchestrator/30-goroutine-dag-orchestrator.md) | advanced | 55m |

---

## 02 - Channels

Master Go's primary communication mechanism: unbuffered and buffered channels, direction constraints, ranging, closing, nil behavior, and the "share memory by communicating" philosophy.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Unbuffered Channel Basics](02-channels/01-unbuffered-channel-basics/01-unbuffered-channel-basics.md) | basic | 15m |
| 2 | [Channel as Synchronization](02-channels/02-channel-as-synchronization/02-channel-as-synchronization.md) | basic | 20m |
| 3 | [Buffered Channels](02-channels/03-buffered-channels/03-buffered-channels.md) | basic | 20m |
| 4 | [Channel Direction](02-channels/04-channel-direction/04-channel-direction.md) | intermediate | 25m |
| 5 | [Ranging Over Channels](02-channels/05-ranging-over-channels/05-ranging-over-channels.md) | intermediate | 20m |
| 6 | [Closing Channels](02-channels/06-closing-channels/06-closing-channels.md) | intermediate | 25m |
| 7 | [Nil Channel Behavior](02-channels/07-nil-channel-behavior/07-nil-channel-behavior.md) | intermediate | 30m |
| 8 | [Channel of Channels](02-channels/08-channel-of-channels/08-channel-of-channels.md) | advanced | 35m |
| 9 | [Buffered Channel as Semaphore](02-channels/09-buffered-channel-as-semaphore/09-buffered-channel-as-semaphore.md) | advanced | 30m |
| 10 | [Channel vs Shared Memory](02-channels/10-channel-vs-shared-memory/10-channel-vs-shared-memory.md) | advanced | 35m |
| 11 | [Channel Error Propagation](02-channels/11-channel-error-propagation/11-channel-error-propagation.md) | intermediate | 30m |
| 12 | [Channel Ownership Patterns](02-channels/12-channel-ownership-patterns/12-channel-ownership-patterns.md) | intermediate | 30m |
| 13 | [Channel Timeout Patterns](02-channels/13-channel-timeout-patterns/13-channel-timeout-patterns.md) | intermediate | 30m |
| 14 | [Channel Pipeline Basics](02-channels/14-channel-pipeline-basics/14-channel-pipeline-basics.md) | intermediate | 30m |
| 15 | [Channel Event Bus](02-channels/15-channel-event-bus/15-channel-event-bus.md) | intermediate | 35m |
| 16 | [Channel State Machine](02-channels/16-channel-state-machine/16-channel-state-machine.md) | advanced | 40m |
| 17 | [Streaming Backpressure](02-channels/17-channel-streaming-backpressure/17-channel-streaming-backpressure.md) | advanced | 40m |
| 18 | [Multi-Producer Single-Consumer](02-channels/18-multi-producer-single-consumer/18-multi-producer-single-consumer.md) | intermediate | 30m |
| 19 | [Channel Orchestration](02-channels/19-channel-orchestration/19-channel-orchestration.md) | intermediate | 35m |
| 20 | [Bounded Work Queue](02-channels/20-bounded-work-queue/20-bounded-work-queue.md) | intermediate | 30m |
| 21 | [Channel Circuit Breaker](02-channels/21-channel-circuit-breaker/21-channel-circuit-breaker.md) | advanced | 40m |
| 22 | [Channel Request Multiplexer](02-channels/22-channel-request-multiplexer/22-channel-request-multiplexer.md) | advanced | 40m |
| 23 | [Channel Request-Response (RPC)](02-channels/23-channel-request-response/23-channel-request-response.md) | advanced | 40m |
| 24 | [Channel Retry with Backoff](02-channels/24-channel-retry-backoff/24-channel-retry-backoff.md) | advanced | 40m |
| 25 | [Ordered Fan-Out Results](02-channels/25-ordered-fan-out-results/25-ordered-fan-out-results.md) | advanced | 45m |
| 26 | [Channel Saga / Compensation](02-channels/26-channel-saga-compensation/26-channel-saga-compensation.md) | advanced | 45m |
| 27 | [Channel Priority Queue](02-channels/27-channel-priority-queue/27-channel-priority-queue.md) | advanced | 40m |
| 28 | [Bridge Channel Pattern](02-channels/28-channel-bridge-pattern/28-channel-bridge-pattern.md) | intermediate | 35m |
| 29 | [Channel Load Balancer](02-channels/29-channel-load-balancer/29-channel-load-balancer.md) | advanced | 45m |
| 30 | [Channel Broadcast](02-channels/30-channel-broadcast/30-channel-broadcast.md) | advanced | 40m |
| 31 | [Channel Resource Pool](02-channels/31-channel-resource-pool/31-channel-resource-pool.md) | advanced | 45m |
| 32 | [Channel Request Coalescing](02-channels/32-channel-request-coalescing/32-channel-request-coalescing.md) | advanced | 40m |

---

## 03 - Select and Multiplexing

Use Go's select statement for non-blocking operations, timeouts, priority tricks, done-channel patterns, heartbeats, and multiplexing multiple sources.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Select Basics](03-select-and-multiplexing/01-select-basics/01-select-basics.md) | basic | 15m |
| 2 | [Select with Default](03-select-and-multiplexing/02-select-with-default/02-select-with-default.md) | basic | 20m |
| 3 | [Select with Timeout](03-select-and-multiplexing/03-select-with-timeout/03-select-with-timeout.md) | intermediate | 25m |
| 4 | [Select Priority Trick](03-select-and-multiplexing/04-select-priority-trick/04-select-priority-trick.md) | intermediate | 30m |
| 5 | [Select in For Loop](03-select-and-multiplexing/05-select-in-for-loop/05-select-in-for-loop.md) | intermediate | 25m |
| 6 | [Done Channel Pattern](03-select-and-multiplexing/06-done-channel-pattern/06-done-channel-pattern.md) | intermediate | 25m |
| 7 | [Heartbeat with Select](03-select-and-multiplexing/07-heartbeat-with-select/07-heartbeat-with-select.md) | advanced | 35m |
| 8 | [Multiplexing N Sources](03-select-and-multiplexing/08-multiplexing-n-sources/08-multiplexing-n-sources.md) | advanced | 40m |
| 9 | [Select with Context](03-select-and-multiplexing/09-select-with-context/09-select-with-context.md) | intermediate | 35m |
| 10 | [Select Deadlock Prevention](03-select-and-multiplexing/10-select-deadlock-prevention/10-select-deadlock-prevention.md) | advanced | 40m |

---

## 04 - Sync Primitives

Explore the sync package: Mutex, RWMutex, WaitGroup, Once, Pool, Cond, and sync.Map. Learn when to choose locks over channels and how to avoid deadlocks.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Mutex: Protect Shared State](04-sync-primitives/01-mutex-protect-shared-state/01-mutex-protect-shared-state.md) | basic | 15m |
| 2 | [RWMutex: Readers-Writers](04-sync-primitives/02-rwmutex-readers-writers/02-rwmutex-readers-writers.md) | intermediate | 30m |
| 3 | [WaitGroup: Wait for All](04-sync-primitives/03-waitgroup-wait-for-all/03-waitgroup-wait-for-all.md) | basic | 20m |
| 4 | [Once: Singleton Initialization](04-sync-primitives/04-once-singleton-init/04-once-singleton-init.md) | intermediate | 25m |
| 5 | [Pool: Object Reuse](04-sync-primitives/05-pool-object-reuse/05-pool-object-reuse.md) | intermediate | 30m |
| 6 | [Cond: Signal and Broadcast](04-sync-primitives/06-cond-signal-broadcast/06-cond-signal-broadcast.md) | advanced | 35m |
| 7 | [Mutex vs Channel: Decision Criteria](04-sync-primitives/07-mutex-vs-channel-decision/07-mutex-vs-channel-decision.md) | intermediate | 25m |
| 8 | [Nested Locking and Deadlock](04-sync-primitives/08-nested-locking-deadlock/08-nested-locking-deadlock.md) | advanced | 35m |
| 9 | [sync.Map: Concurrent Access](04-sync-primitives/09-sync-map-concurrent-access/09-sync-map-concurrent-access.md) | intermediate | 25m |
| 10 | [Build a Thread-Safe Counter](04-sync-primitives/10-build-thread-safe-counter/10-build-thread-safe-counter.md) | advanced | 40m |
| 11 | [WaitGroup Patterns](04-sync-primitives/11-waitgroup-patterns/11-waitgroup-patterns.md) | advanced | 40m |
| 12 | [Mutex Granularity](04-sync-primitives/12-mutex-granularity/12-mutex-granularity.md) | advanced | 45m |

---

## 05 - Atomic and Memory Ordering

Work with the sync/atomic package for lock-free programming: Add, Load/Store, CompareAndSwap, atomic.Value, spinlocks, and happens-before semantics.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Atomic Add Counter](05-atomic-and-memory-ordering/01-atomic-add-counter/01-atomic-add-counter.md) | basic | 25m |
| 2 | [Atomic Load and Store](05-atomic-and-memory-ordering/02-atomic-load-store/02-atomic-load-store.md) | intermediate | 25m |
| 3 | [Atomic Compare-And-Swap](05-atomic-and-memory-ordering/03-atomic-compare-and-swap/03-atomic-compare-and-swap.md) | intermediate | 30m |
| 4 | [Atomic Value for Dynamic Configuration](05-atomic-and-memory-ordering/04-atomic-value-dynamic-config/04-atomic-value-dynamic-config.md) | intermediate | 30m |
| 5 | [Spinlock with Atomic CAS](05-atomic-and-memory-ordering/05-spinlock-with-atomic/05-spinlock-with-atomic.md) | advanced | 35m |
| 6 | [Happens-Before Guarantees](05-atomic-and-memory-ordering/06-happens-before-guarantees/06-happens-before-guarantees.md) | advanced | 35m |
| 7 | [Atomic vs Mutex Benchmark](05-atomic-and-memory-ordering/07-atomic-vs-mutex-benchmark/07-atomic-vs-mutex-benchmark.md) | advanced | 35m |

---

## 06 - Context

Learn the context package for cancellation, timeouts, deadlines, and value propagation across API boundaries and goroutine trees.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Context Background and TODO](06-context/01-context-background-and-todo/01-context-background-and-todo.md) | basic | 20m |
| 2 | [Context WithCancel](06-context/02-context-withcancel/02-context-withcancel.md) | basic | 25m |
| 3 | [Context WithTimeout](06-context/03-context-withtimeout/03-context-withtimeout.md) | intermediate | 25m |
| 4 | [Context WithDeadline](06-context/04-context-withdeadline/04-context-withdeadline.md) | intermediate | 25m |
| 5 | [Context WithValue](06-context/05-context-withvalue/05-context-withvalue.md) | intermediate | 30m |
| 6 | [Context Propagation Chain](06-context/06-context-propagation-chain/06-context-propagation-chain.md) | intermediate | 35m |
| 7 | [Context-Aware Long Worker](06-context/07-context-aware-long-worker/07-context-aware-long-worker.md) | advanced | 35m |
| 8 | [Graceful Shutdown with Context](06-context/08-graceful-shutdown-with-context/08-graceful-shutdown-with-context.md) | advanced | 40m |

---

## 07 - Concurrency Patterns

Implement the classic Go concurrency patterns: pipelines, fan-out/fan-in, worker pools, semaphores, generators, or-channel, tee-channel, and rate limiters.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Pipeline Pattern](07-concurrency-patterns/01-pipeline-pattern/01-pipeline-pattern.md) | intermediate | 30m |
| 2 | [Fan-Out: Distribute Work](07-concurrency-patterns/02-fan-out-distribute-work/02-fan-out-distribute-work.md) | intermediate | 30m |
| 3 | [Fan-In: Merge Results](07-concurrency-patterns/03-fan-in-merge-results/03-fan-in-merge-results.md) | intermediate | 30m |
| 4 | [Worker Pool (Fixed)](07-concurrency-patterns/04-worker-pool-fixed/04-worker-pool-fixed.md) | intermediate | 35m |
| 5 | [Semaphore: Bounded Concurrency](07-concurrency-patterns/05-semaphore-bounded-concurrency/05-semaphore-bounded-concurrency.md) | intermediate | 30m |
| 6 | [Generator: Lazy Production](07-concurrency-patterns/06-generator-lazy-production/06-generator-lazy-production.md) | intermediate | 25m |
| 7 | [Or-Channel: First to Finish](07-concurrency-patterns/07-or-channel-first-to-finish/07-or-channel-first-to-finish.md) | advanced | 35m |
| 8 | [Tee-Channel: Split Stream](07-concurrency-patterns/08-tee-channel-split-stream/08-tee-channel-split-stream.md) | advanced | 30m |
| 9 | [Rate Limiter: Token Bucket](07-concurrency-patterns/09-rate-limiter-token-bucket/09-rate-limiter-token-bucket.md) | advanced | 35m |
| 10 | [End-to-End Pipeline with Cancellation](07-concurrency-patterns/10-end-to-end-pipeline-with-cancel/10-end-to-end-pipeline-with-cancel.md) | advanced | 45m |

---

## 08 - Errgroup

Use golang.org/x/sync/errgroup for structured concurrency with error propagation, context-aware groups, concurrency limits, and result collection.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Errgroup Basics](08-errgroup/01-errgroup-basics/01-errgroup-basics.md) | basic | 25m |
| 2 | [Errgroup with Context](08-errgroup/02-errgroup-with-context/02-errgroup-with-context.md) | intermediate | 30m |
| 3 | [Errgroup SetLimit](08-errgroup/03-errgroup-setlimit/03-errgroup-setlimit.md) | intermediate | 25m |
| 4 | [Errgroup Collect Results](08-errgroup/04-errgroup-collect-results/04-errgroup-collect-results.md) | intermediate | 30m |
| 5 | [Errgroup vs WaitGroup](08-errgroup/05-errgroup-vs-waitgroup/05-errgroup-vs-waitgroup.md) | intermediate | 25m |
| 6 | [Errgroup Parallel Pipeline](08-errgroup/06-errgroup-parallel-pipeline/06-errgroup-parallel-pipeline.md) | advanced | 40m |

---

## 09 - Data Races and Detector

Learn to identify, detect, and fix data races using Go's race detector, mutexes, channels, atomics, and race-free design patterns.

| # | Exercise | Difficulty | Time |
|---|----------|------------|------|
| 1 | [Your First Data Race](09-data-races-and-detector/01-your-first-data-race/01-your-first-data-race.md) | basic | 20m |
| 2 | [Race Detector Flag](09-data-races-and-detector/02-race-detector-flag/02-race-detector-flag.md) | basic | 20m |
| 3 | [Fix Race with Mutex](09-data-races-and-detector/03-fix-race-with-mutex/03-fix-race-with-mutex.md) | intermediate | 25m |
| 4 | [Fix Race with Channel](09-data-races-and-detector/04-fix-race-with-channel/04-fix-race-with-channel.md) | intermediate | 25m |
| 5 | [Fix Race with Atomic](09-data-races-and-detector/05-fix-race-with-atomic/05-fix-race-with-atomic.md) | intermediate | 25m |
| 6 | [Subtle Race: Map Access](09-data-races-and-detector/06-subtle-race-map-access/06-subtle-race-map-access.md) | intermediate | 30m |
| 7 | [Race in Closure Loops](09-data-races-and-detector/07-race-in-closure-loops/07-race-in-closure-loops.md) | intermediate | 25m |
| 8 | [Race-Free Design Patterns](09-data-races-and-detector/08-race-free-design-patterns/08-race-free-design-patterns.md) | advanced | 40m |
