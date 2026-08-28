# ADR-050 implementation notes

Binding constraints on the nvs-api-server implementation, recorded during
the review of [ADR-050](050-datastore-api-server.md) so they are not
rediscovered. Nothing here changes a decision; the decisions and accepted
trade-offs live in the ADR.

## Ordering and clocks

- Arrival-derived ordering values fail retries: a batch whose OK reply was
  lost is retried later, possibly on another replica, and a receive time
  (or a clamp against one) grows between attempts, letting old state
  overwrite newer state. The database's idempotency key protects stored
  documents, not condition side effects. Hence the event's own timestamp.
- `generatedTimestamp` is optional today and the current condition code
  falls back to the wall clock when it is missing, which is exactly the
  retry instability the design removes; hence missing or invalid
  timestamps store without condition updates.
- The skew check runs per attempt because the server is stateless and a
  retry is a new request, so eligibility can differ between attempts of
  one batch. That is safe: the bounds and the strictly-newer guard hold at
  whichever attempt applies. The accepted fast-clock case: an event
  rejected as too far ahead on its first attempt becomes eligible as wall
  time catches up and can then win against a correctly stamped event it
  semantically follows; the disturbance is bounded by the skew and
  realized only while the batch still retries, so the cap is the retry
  window plus the future bound. Removing it would take durable per-batch
  admission state, deliberately avoided.
- Events from a node's own publishers share its clock and compare exactly;
  a cross-node publisher stamps target-node events with its own host
  clock, so those comparisons carry the bounded skew. During the migration
  overlap, both writers order by the same event timestamps.
- The past bound doubles as replay hygiene: backlog delayed through an
  outage lands in the database without thrashing conditions that fresher
  events have already updated.

## Admission accounting

- Byte accounting: admission charges the request's uncompressed protobuf
  size times a fixed overhead multiplier covering decoding, queue wrappers
  and pipeline metadata growth. The multiplier is configuration, validated
  against the retained-bytes gauge; the reservation is taken before any
  transformation runs, and a rejected batch is never transformed.
- Decoded pre-admission requests: gRPC unmarshals a unary request before
  any interceptor runs, so admission cannot prevent the decode. A
  concurrent-request cap bounds the aggregate (cap times
  `maxRequestBytes`), covering requests waiting on TokenReview or
  admission, and that product is part of the memory model.
- Client backoff jitter: the reused retry delay caps at a few
  deterministic seconds; a hundred thousand clients retrying in step on
  that schedule would be an outage of its own.
- Memory model: the queued-bytes bound, token caches, tens of thousands of
  HTTP/2 connections, the node informer, dedup state, and database driver
  buffers put a replica at roughly 2 GiB at the target scale; the chart's
  resource requests are derived from these knobs.

## Authentication capacity mechanics

- Wave working set: a 2,500 per second wave over a 120 second window is
  about 300,000 reviews, the full current-phase population visiting all
  three replicas, so the per-replica cache working set is the full token
  population. An LRU that evicts still-valid entries turns the
  once-per-window review bound into repeated reviews.
- Misses for the same token coalesce: one in-flight review per token, with
  concurrent requests waiting on its result (the validator has no such
  coalescing today). An in-flight authentication cap bounds the requests
  that can wait out the 8 second retry window, rejecting beyond it.
- The proof of concept's assembly used a 50 QPS Kubernetes client, which
  would fail closed at the target. Connection-age jitter that spreads
  steady reconnects also spreads a rollout's cache warm-up.

## Central k8s connector mechanics

- The reused consume loops are single-worker; the central pools partition
  k8s work by node name to preserve per-node ordering, while store workers
  run concurrently under per-event idempotency.
- The Kubernetes client rate limits are node-sized today and rise to fleet
  rates centrally. The node-Event name cache is a 1,024-entry constant
  today; at fleet scale constant eviction would recreate Event objects
  instead of updating the existing series, so it gets key and byte
  capacities.
- Node metadata lookup today is a 50-entry cache with misses serialized
  behind one lock; centrally it becomes a shared informer with a transform
  that keeps only the fields the augmentor reads (tens of megabytes at
  fleet scale rather than full Node objects), and conflict recovery keeps
  its live reads.
- Rolling updates serialize drains only with `maxSurge: 0` and
  `maxUnavailable: 1`: a terminating pod counts as unavailable, so the
  next deletion waits for the draining pod, whereas a surge rollout starts
  terminating another old pod as soon as a replacement is ready.

## Trace stamping

The handler captures the caller's span context when it enqueues a batch,
and the store connector stamps each stored document from a span linked to
that capture. In the proof of concept, which had no extraction wired, the
stored IDs were all zeroes.

## Tuning values

```yaml
nvsApiServer:
  ingest:
    maxQueuedBatches: 50000   # admission bound, items (queued + in flight + retrying)
    maxQueuedBytes: 512Mi     # admission bound, accounted in-memory bytes
    maxRequestBytes: 4Mi      # gRPC per-request size limit
    maxConcurrentRequests: 512  # bounds decoded pre-admission requests
    overheadMultiplier: 3     # accounted bytes = uncompressed proto size x this
    perNodeMaxBatches: 64     # per-node cap inside the bound
    perNodeMaxBytes: 4Mi      # per-node byte cap inside the bound
    crossNodeMaxEvents: 5000  # per cross-node identity, in events (the fan-out unit)
    maxEventsPerBatch: 1000   # also caps cross-node fan-out per request
    maxNodesPerBatch: 256     # distinct nodes one batch may name
  auth:
    callerCacheEntries: 150000      # full token population + rotation overlap;
                                    # a reconnect wave can bring every token to each replica
    attachmentCacheEntries: 64      # cross-node identities only
    tokenReviewQPS: 2000            # per replica, covers the reconnect wave with headroom
    tokenReviewBurst: 4000
```
