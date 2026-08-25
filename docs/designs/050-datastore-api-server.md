# ADR-050: Datastore API Server and Pass-Through Proxy

## Context

Every NVSentinel component that needs the datastore talks to MongoDB (or
PostgreSQL) through the shared `store-client` library. Most of these components
are small, single-replica Deployments. One of them is not: platform-connectors
runs as a DaemonSet, one pod on every node, and every pod opens its own
connections to the database.

Each platform-connector pod holds about 3 MongoDB connections: heartbeat
connections that the driver keeps to each replica set member, plus the
connections used for actual writes. The database pays memory (roughly 0.2 MB)
for every open connection, even an idle one. Because the pod count grows with
the fleet, the connection count grows with it:

| Fleet size    | Connections from platform-connectors |
|---------------|---------------------------------------|
| 1,000 nodes   | ~3,000                                |
| 10,000 nodes  | ~30,000                               |
| 100,000 nodes | ~300,000                              |

At 100k nodes, MongoDB spends about 61 GiB of memory just keeping those
connections open, before storing a single byte of data
([issue #1595](https://github.com/NVIDIA/NVSentinel/issues/1595)).

What makes this fixable is how little each pod does with those connections.
The platform-connector store connector
(`platform-connectors/pkg/connectors/store/store_connector.go`) performs
exactly one operation: batched inserts of health events. It never reads.

The other datastore clients are the opposite: few in number, rich in usage.
Six single-replica Deployments (fault-quarantine, node-drainer,
health-events-analyzer, fault-remediation, event-exporter, csp-health-monitor)
use change streams, resume tokens, finds, updates and aggregations. Together
they hold a small, constant number of connections (~20) no matter how large
the fleet is.

```text
 Today: every platform-connector pod connects straight to the database

   node 1      node 2      node 3              node N
  +------+    +------+    +------+            +------+
  |  PC  |    |  PC  |    |  PC  |    ....    |  PC  |
  +------+    +------+    +------+            +------+
      |           |           |                   |
      | ~3        | ~3        | ~3                | ~3 connections each
      v           v           v                   v
  +----------------------------------------------------+
  |                      MongoDB                       |
  |     3 x N connections, growing with the fleet      |
  +----------------------------------------------------+
                           ^
                           |  ~20 connections, constant
                six central services
```

Issue #1595 originally suggested deploying
[mongobetween](https://github.com/coinbase/mongobetween), a third party
MongoDB connection pooler by Coinbase. This ADR proposes building our own
component instead, and includes a review of what mongobetween teaches us and
which parts of it are worth reusing (see "What we take from mongobetween"
below).

Options considered:

1. Deploy `mongobetween` as-is.
2. Build an NVSentinel datastore api-server with two modes: a gRPC write API
   for platform-connectors and a byte-level pass-through tunnel for the
   central services. This is the proposal.
3. Build only one of the two modes: either a gRPC API for every client, or a
   tunnel for every client.

### Background: a middleman can do one of two jobs

A middleman between clients and a database can work in one of two ways, and
the difference decides what it can and cannot fix.

The first way is a **pass-through tunnel**. The middleman accepts a client
connection, opens its own connection to the database, and copies bytes in both
directions without understanding them. Everything keeps working through it
(change streams, cursors, transactions, end-to-end TLS), because nothing about
the database protocol changes. But a tunnel is always one client connection to
one database connection. It cannot merge two clients onto a shared connection,
because merging requires understanding where one request ends and which reply
belongs to which client. A tunnel therefore gives you a single controlled
doorway, but no reduction in connection count.

The second way is an **API in front of the database**. The client never talks
to the database at all: it sends a request to the middleman ("store these 5
health events, here is my token"), and the middleman checks the token, then
performs the database write itself using a small pool of connections it owns.
Because it understands requests, it can funnel 100k clients through a handful
of database connections. The cost is that it only supports the operations you
teach it.

```text
 Pass-through tunnel: every client pipe becomes one database pipe

   client A =====[            ]===== database
   client B =====[   tunnel   ]===== database
   client C =====[            ]===== database

   3 connections in, 3 connections out. The tunnel never reads the
   bytes, so everything keeps working, but nothing is saved.

 API in front: client conversations end at the middleman

   client A --request-->+---------+
   client B --request-->|   API   |=== small fixed pool ===> database
   client C --request-->+---------+

   many clients in, a handful of connections out. The API reads
   every request, so it can check tokens and share connections,
   but it only supports the operations it was taught.
```

The fleet-scaling client (platform-connectors) needs the API treatment,
because that is the only way to collapse connections. The six central services
only need the tunnel treatment, because their connection count is already
small and their database usage is too rich to re-implement behind an API.

## Decision

Build a new service, the **datastore-api-server**, and make it the single
network path to the datastore. It exposes two ports: a gRPC write API that
platform-connectors uses to store health events, authenticated on every
request with projected ServiceAccount tokens, and a TCP pass-through tunnel
that the central services use for full database access, authenticated once per
connection with the same token mechanism.

## Implementation

### Architecture

```text
 platform-connector pods                 central services (6 Deployments,
 (DaemonSet, one per node,               1 replica each: fault-quarantine,
  grows with the fleet)                  node-drainer, health-events-analyzer,
        |                                fault-remediation, event-exporter,
        | gRPC HealthEventOccurredV1     csp-health-monitor)
        | + SA token on every request           |
        | one HTTP/2 connection per pod         | SA token handshake, then
        |                                       | raw MongoDB wire bytes
        v                                       v
 +--------------------------------------------------------------+
 |                     datastore-api-server                      |
 |                (Deployment, small fixed size)                 |
 |                                                               |
 |  :50051  gRPC write API                                       |
 |          validate token via TokenReview, then write through   |
 |          store-client using a small shared connection pool    |
 |                                                               |
 |  :27017  pass-through tunnel                                  |
 |          validate token once, then copy bytes 1:1             |
 +--------------------------------------------------------------+
        |                                       |
        | pooled connections                    | tunneled connections,
        | (bounded, ~10 per replica)            | one per client connection
        v                                       v
                    MongoDB (or PostgreSQL)
```

Database connections become constant: pooled write connections plus the ~20
tunneled connections from central services, regardless of fleet size. At 100k
nodes this is roughly 300,000 connections down to roughly 50.

### The write API

The api-server implements the existing `PlatformConnector` gRPC service
(`data-models/protobufs/health_event.proto`, `HealthEventOccurredV1`). No new
service definition is needed: this is the same service that
platform-connectors itself serves, and the same one the gRPC sink connector
(ADR-033) already knows how to call from the client side.

On the platform-connectors side, the store path gets a new mode. Instead of
the store connector writing to the database through `store-client`, it sends
the same batches to the api-server over gRPC. The existing gRPC sink connector
code is the starting point for this client, with one important difference in
delivery guarantees:

- The optional external sink (ADR-033) drops a batch after `maxRetries`
  attempts. That is acceptable for a side feed.
- The store path is the primary persistence path and must not drop events. It
  keeps retrying with backoff and relies on the existing ring buffer for
  backpressure, matching the behavior of the direct store connector today.

Because the client retries, a batch could be delivered twice if a write
succeeded but the acknowledgement was lost. Each batch therefore carries a
client-generated idempotency ID (in gRPC metadata, or as a new optional field
on `HealthEvents`; either stays compatible with existing callers), and the
api-server refuses to apply the same batch twice. The duplicate check must
live in the datastore itself: a retried batch may arrive at a different
api-server replica, so an in-memory record of applied IDs on one replica
cannot catch it. Concretely, each event in a batch gets a unique key derived
from the batch ID and its position in the batch, a unique index enforces it,
and inserts run unordered with duplicate-key results treated as success. That
way a retry, on any replica, even after a batch was only partially applied,
stores exactly the missing events and nothing twice.

```text
 platform-connector             api-server                  database
        |                           |                           |
        | HealthEventOccurredV1     |                           |
        | events + batch ID,        |                           |
        | SA token in metadata      |                           |
        |-------------------------->|                           |
        |                           | validate token            |
        |                           | (TokenReview, cached),    |
        |                           | check writer allowlist    |
        |                           |                           |
        |                           | store-client InsertMany   |
        |                           |-------------------------->|
        |                           |<----- acknowledged -------|
        |<--------- OK -------------|                           |
        |                           |                           |
   the batch leaves the ring buffer only after OK; if the OK is
   lost, the retry carries the same batch ID and the api-server
   refuses to store it twice
```

On the api-server side, the handler validates the caller (next section) and
then calls `store-client` `InsertMany`, exactly as the store connector does
today. Event transformation and deduplication stay in the platform-connector
pipeline, unchanged; the api-server is a thin persistence endpoint. The
api-server configures an explicit `maxPoolSize` on its database client.
(Today no component sets pool limits for MongoDB; the driver default of 100
applies. Concentrating writes in one place is what makes an explicit small
pool meaningful.)

Two practical notes for scale:

- Each platform-connector pod holds a single HTTP/2 connection to the
  api-server, carrying all its calls. 100k idle HTTP/2 connections are cheap
  compared to 300k MongoDB connections, and they terminate at the api-server,
  not at the database.
- The api-server sets a gRPC `MaxConnectionAge` so long-lived client
  connections periodically reconnect and spread evenly across replicas when
  the api-server scales.

### Authentication on the write API

This reuses the ServiceAccount token machinery that already authenticates
health event publishers to platform-connectors
(`docs/configuration/authentication.md`) and the janitor to the
janitor-provider (ADR-030):

- Clients attach a projected, audience-scoped, short-lived ServiceAccount
  token to every request using `commons/pkg/grpcclient`.
- The api-server validates tokens with the Kubernetes TokenReview API using
  `commons/pkg/grpcauth` (including its result cache), with a dedicated
  audience, for example `datastore-api-server.nvsentinel.nvidia.com`.
- The api-server only accepts writes from an allowlist of identities, by
  default just the platform-connector ServiceAccount derived from the release
  namespace, the same derivation pattern the publisher auth uses.
- Tokens must be pod-bound, so a token extracted from a manifest or minted
  without a pod reference is refused.

Per-event node binding (a caller may only report events about its own node) is
already enforced where it belongs: at the platform-connector layer, which
knows which node each publisher runs on. The api-server does not re-check it,
because a platform-connector pod legitimately forwards cross-node events that
were accepted upstream from allowlisted publishers. A compromised
platform-connector pod can therefore still write events for other nodes, which
is exactly the access it has today with direct database credentials; this
change does not widen it, and the caller allowlist narrows who can write at
all.

### The pass-through tunnel

The api-server listens on a second port and tunnels raw database traffic:

1. The client opens a TCP connection and sends one small preamble frame
   containing its projected ServiceAccount token and the database address
   (`host:port`) it wants to reach.
2. The api-server validates the token via TokenReview (same audience
   machinery, once per connection instead of once per request), checks that
   the requested address is one of its configured database backends, and
   answers OK. Without the address check, the tunnel would be an
   authenticated relay to anywhere on the network.
3. From then on the api-server copies bytes in both directions and understands
   nothing about them. One client connection maps to one database connection.

```text
 central service                api-server                  database
        |                           |                           |
        | TCP connect               |                           |
        |-------------------------->|                           |
        | preamble: SA token +      |                           |
        | database address          |                           |
        |-------------------------->|                           |
        |                           | validate token            |
        |                           | (TokenReview, once per    |
        |                           |  connection), check the   |
        |                           |  address is a known       |
        |                           |  database backend         |
        |<--------- OK -------------|                           |
        |                           | TCP connect to that       |
        |                           | address                   |
        |                           |-------------------------->|
        |                           |                           |
        |  MongoDB wire protocol, TLS and X.509 auth end to end |
        |<=====================================================>|
        |     bytes copied both ways, never read or changed     |
```

Everything the central services rely on keeps working, because nothing about
the database protocol changes: change streams, resume tokens, aggregations,
transactions, and MongoDB's own end-to-end TLS with X.509 client certificates
(`MONGODB-X509` support lives in `store-client` today and is untouched; when
TLS is on, the api-server cannot read the tunneled traffic, which is fine
because it does not need to).

The client side of the preamble lives in `store-client` as a custom dialer
(the MongoDB driver exposes a hook for exactly this, and the PostgreSQL
driver has an equivalent). The dialer matters more than it looks. When
connecting to a replica set, the driver does not only dial the address it was
given: it discovers the member addresses from the replica set itself and
dials each member directly. Simply pointing the connection string at the
api-server would therefore tunnel the first connection and bypass the tunnel
for all the discovered ones. With the dialer, every connection the driver
opens, to whatever address it discovered, goes through the tunnel, and the
preamble names the real destination. Nothing else changes for the client:
the connection string, TLS hostname verification and X.509 authentication
all still refer to the real database members. Central services adopt the
tunnel by upgrading `store-client` and setting one variable with the tunnel
address. A tunnel connection can also outlive the token that opened it; that
mirrors how a database session outlives the credential check done at login.

NetworkPolicies restricting both api-server ports to known NVSentinel pods,
and restricting the database to accept connections only from the api-server,
are recommended as defense in depth, consistent with ADR-033.

One transit detail stated plainly: the token itself crosses the pod network
on both doors (in gRPC metadata on the write API, in the preamble on the
tunnel, where it is sent before any TLS exists on that leg). Like the rest of
NVSentinel's internal gRPC today, these legs are unencrypted by default; the
token's own protections (short lifetime, single audience, pod binding) plus
the NetworkPolicies bound what a captured token is worth. Deployments that
want encryption on these legs can enable server TLS on both api-server ports
using the same cert-manager pattern as ADR-030. On the tunnel, the whole
connection including the preamble is then wrapped in TLS to the api-server,
and the database's own end-to-end TLS simply runs inside it; double
encryption on ~20 connections is a negligible cost.

### What we take from mongobetween

We reviewed the [mongobetween](https://github.com/coinbase/mongobetween)
source to decide what is reusable. It is Apache 2.0 licensed, the same license
as NVSentinel, so reusing code is permitted with attribution.

Its design in one paragraph: it terminates client connections, parses each
MongoDB wire message just enough to learn the command, cursor ID and session
ID, answers `ismaster` handshakes itself by pretending to be a `mongos` shard
router (so client drivers stop monitoring replica set members and stop opening
heartbeat connections), and forwards everything else through the official Go
driver's internal connection pool, pinning cursors and transactions to the
backend server that owns them.

What we take from it:

- **Proof the shape works.** Coinbase runs it in production and reports
  connection spikes of 30k collapsing to about 2k. That is the same pattern as
  our write API: many cheap client connections in, one small bounded driver
  pool out, with the official driver doing pooling, server selection and
  failover. We need no custom pooling code; the ordinary driver pool inside
  `store-client` already is the pool.
- **The metrics catalogue.** Its metric set (message handling time, backend
  round-trip time, open client connections, driver pool checkout events,
  tracked cursors and transactions) is a ready checklist for the api-server's
  Prometheus metrics.
- **The migration trick.** Its dynamic configuration can disable writes or
  redirect an address at runtime, which Coinbase used for low-downtime cluster
  migrations. Not part of this design, but worth remembering if we ever need a
  datastore write freeze or move.

What we deliberately do not take:

- **The wire protocol parsing and multiplexing core.** Our write API speaks
  gRPC, so none of it applies there. Our tunnel deliberately does not parse
  traffic, so none of it applies there either. That core is only needed for a
  middle road we are not taking (a pooled MongoDB-protocol endpoint), and it
  is also where the sharp edges live: it extracts the Go driver's private
  topology object using reflection and unsafe pointers (which breaks across
  driver upgrades), and its fake handshake is frozen at MongoDB wire version 8
  (the MongoDB 4.2 era), silently capping what features clients negotiate.
- **Its authentication model, which is the opposite of what we need.** Clients
  cannot authenticate through mongobetween at all: the fake handshake
  advertises an empty `saslSupportedMechs` list (the source comments "proxy
  doesn't support auth"). Database credentials move into the proxy, and
  anyone who can reach the proxy socket can use them. NVSentinel needs the
  reverse: callers proving who they are with ServiceAccount tokens, and, on
  the tunnel, MongoDB's own authentication still flowing end to end.

If a pooled MongoDB-protocol endpoint ever becomes necessary (for example if
the central services ever scale out), mongobetween is the reference to borrow
from: the fake-router handshake and the small cursor-to-server and
session-to-server pinning caches are the hard-won pieces, and the license
allows lifting them. That is future work, not this design.

### Scaling and availability

The api-server scales horizontally, and two design choices above exist to
keep that true:

- It is stateless. All durable state, including the idempotency check, lives
  in the database, so any replica can serve any request and a retried batch
  can land on any replica.
- `MaxConnectionAge` keeps redistributing the long-lived client connections,
  so a newly added replica picks up its share of load within minutes instead
  of sitting idle behind connections pinned to older replicas.

Adding a replica costs the database a small, fixed amount: its bounded pool
(~10 connections) plus a few heartbeat connections, call it ~13. Capacity
therefore grows with replica count while database connections stay a small
multiple of it, and replica count grows with load, never with fleet size. A
horizontal pod autoscaler is possible later; a fixed replica count is enough
to start.

Two boundaries worth stating plainly:

- Tunnel connections are pinned to the replica that carries them; a live
  byte splice cannot be moved. Scaling out helps new connections only, and
  scaling in (or restarting a replica) snaps the tunnels it carries. The
  clients' drivers redial through a surviving replica and change streams
  resume from their tokens, so the effect is a brief reconnect, bounded by
  the PodDisruptionBudget and graceful drain during rollouts.
- Token validation caches are per replica, so a mass reconnect wave briefly
  multiplies TokenReview calls to the Kubernetes API server. The
  `commons/pkg/grpcauth` cache bounds this, and the existing auth metrics
  already distinguish an unavailable validator from real auth failures.

### Helm and configuration

A new deployment in the NVSentinel chart, disabled by default:

```yaml
datastoreApiServer:
  enabled: false
  replicas: 3
  grpcPort: 50051
  passthroughPort: 27017
  auth:
    audience: "datastore-api-server.nvsentinel.nvidia.com"
    tokenExpirationSeconds: 3600
  datastore:
    maxPoolSize: 10   # database connections per replica
```

Client sides are Helm switches, so rollout and rollback are value flips:

- Central services: a tunnel address variable, rendered next to the existing
  datastore settings in `configmap-datastore.yaml`, turns the `store-client`
  dialer on. The connection string itself stays untouched.
- Platform-connectors: a store connector mode selects direct database writes
  (today's behavior, the default) or writes via the api-server.

### Rollout plan

1. Ship the api-server disabled. Nothing changes.
2. Enable it and turn on the tunnel dialer for the central services. This is
   behavior-neutral by construction (same protocol, same 1:1 connections, one
   extra hop) and validates the api-server on the read path with low risk.
3. Switch the platform-connectors store path to the gRPC write API. This is
   the step where database connections collapse.
4. Rollback at any step is flipping the values back; direct database access
   keeps working throughout the migration.

```text
 Phase 1: ship disabled          nothing changes
   PC -------------------------------------------> database
   central services -----------------------------> database

 Phase 2: tunnel first           same protocol, one extra hop
   PC -------------------------------------------> database
   central services ----> [api-server] ==========> database

 Phase 3: write API              connections collapse
   PC --gRPC + token----> [api-server] --pool----> database
   central services ----> [api-server] ==========> database
```

## Rationale

- Database connections become constant instead of linear in fleet size:
  roughly 300,000 down to roughly 50 at 100k nodes, freeing about 60 GiB of
  database memory.
- The pieces already exist and are proven: the `PlatformConnector` gRPC
  service and its client (ADR-033), the ServiceAccount token auth stack from
  the publisher auth work and ADR-030 (`commons/pkg/grpcauth`,
  `commons/pkg/grpcclient`), and `store-client` for the actual writes.
- Database credentials concentrate in one small Deployment. Today, in
  deployments with X.509 auth, every node's platform-connector pod carries a
  database client certificate; after this change only the api-server and the
  six central services do, and rotation on the write path touches 3 pods
  instead of the whole fleet.
- The write path becomes datastore-agnostic on the wire: platform-connectors
  no longer knows or cares whether the backend is MongoDB or PostgreSQL. The
  PostgreSQL provider benefits equally (its per-client pool is hardcoded to
  25 connections, which would scale even worse with the fleet).
- No dependency on an unmaintained third party proxy, and no need to
  understand or re-implement the MongoDB wire protocol.

## Consequences

### Positive

- Connection count and database connection memory stop growing with the fleet.
- Per-request authentication on the write path, using the standard NVSentinel
  token mechanism, where today a stolen database credential is enough.
- One controlled network doorway to the datastore, easy to firewall.
- A migration path: any tunneled client can later move to a purpose-built API
  one at a time, with no big-bang rewrite.

### Negative

- A new component sits on the critical write path. If the api-server is down,
  health events do not reach the database until it returns.
- Every datastore interaction gains one network hop of latency.
- The api-server must handle very many concurrent gRPC connections at large
  fleet sizes and becomes a component to size, monitor and page on.
- The tunnel gives central services no pooling benefit, only a doorway.

### Mitigations

- The platform-connector ring buffer already absorbs store outages: events
  buffer and retry instead of being lost, so an api-server restart behaves
  like a database restart does today. The api-server runs multiple replicas
  with anti-affinity and a PodDisruptionBudget.
- The added hop is microseconds to low milliseconds inside a cluster, against
  a write path whose RPC timeout is 10 seconds; it does not change any
  user-visible latency.
- HTTP/2 connections are cheap to hold, the api-server is stateless (all
  state is in the database), so it scales horizontally; standard gRPC and
  auth metrics (same families as ADR-033 and the publisher auth work, plus
  the mongobetween-inspired catalogue above) make it observable.
- No pooling for central services is accepted: their connection count is ~20
  and constant, so there is nothing to pool away.

## Alternatives Considered

### Deploy mongobetween as-is

**Rejected** because: clients cannot authenticate to it by design (its
handshake advertises no auth mechanisms, so database credentials sit in the
proxy protected only by network reachability, which fails our requirement to
authenticate callers with ServiceAccount tokens); it fronts `mongos` shard
routers and its own README calls direct replica set use not battle tested,
while NVSentinel's default deployment is a replica set; it is MongoDB-only
while NVSentinel also supports PostgreSQL; and it has been unmaintained for
about two years on Go 1.18, reaching into the Go driver's private structures
via reflection and freezing its handshake at MongoDB 4.2's wire version. Its
lessons and parts of its code remain reusable (Apache 2.0); see "What we take
from mongobetween".

### Build only one of the two modes

**Rejected** because: each mode solves a problem the other cannot. A tunnel
for everyone is one client connection to one database connection by
definition, so it cannot reduce the DaemonSet's connection count; it would add
a hop and a component while leaving the actual problem (300k connections)
fully intact. A gRPC API for everyone means re-implementing change streams,
resume tokens and aggregations over gRPC for six central services that
contribute ~20 connections and no scaling pressure. The write API removes the
connections that grow with the fleet; the tunnel carries the rich, low-volume
clients unchanged, and they can migrate to purpose-built APIs later if a
reason appears.

## Notes

- Non-goal: changing the datastore schema, the `store-client` interface, or
  the event pipeline (transformation and deduplication stay in
  platform-connectors).
- Non-goal: a general query API. Reads stay on the pass-through tunnel in
  this design.
- Non-goal: a pooled MongoDB-protocol endpoint (the mongobetween approach).
  Recorded above as possible future work with a known reference
  implementation.
- The external gRPC sink use case from ADR-033 is unchanged; the api-server
  reuses its client pattern but with store-path delivery guarantees.
- Open question for implementation: the exact preamble frame format for the
  tunnel handshake (token, destination address, and possibly a protocol
  version for future use).

## References

- [Issue #1595: Deploy a MongoDB connection proxy to keep connection count constant as fleet scales](https://github.com/NVIDIA/NVSentinel/issues/1595)
- [mongobetween](https://github.com/coinbase/mongobetween) and Coinbase's
  [scaling write-up](https://blog.coinbase.com/scaling-connections-with-ruby-and-mongodb-99204dbf8857)
- [ADR-033: gRPC Sink Connector for Platform-Connectors](033-grpc-sink-connector.md)
- [ADR-030 file: gRPC TLS and Authentication for Janitor-Provider Connection](030-grpc-tls-authentication.md)
- [Publisher authentication reference](../configuration/authentication.md)
- [ADR-002: Storage Layer Selection](002-storage-layer-selection.md)
