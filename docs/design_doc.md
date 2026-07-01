# Design Document: Multi-Region, Fault-Tolerant URL/Content Delivery System

This design document outlines the architecture, distributed systems reasoning, CAP theorem trade-offs, and fault-tolerance modes of the Multi-Region URL/Content Delivery System.

---

## 1. Architectural Topology

The system models a global infrastructure simulated across three geographical regions:
- **US-East** (Virginia): Houses the primary writable database instance and the local Redis cache / regional Go app server.
- **US-West** (Oregon): Houses a read-only database replica, local Redis cache, and regional Go app server.
- **EU-West** (Ireland): Houses a read-only database replica, local Redis cache, and regional Go app server.
- **Global Router** (GSLB Proxy): Fronts all traffic, routing requests based on geographic headers (`X-Client-Region`) and checking backend health.

```
                     +---------------------------------------+
                     |            Client Console             |
                     +---------------------------------------+
                                         |
                                         v
                     +---------------------------------------+
                     |         Global Router (GSLB)          |
                     +---------------------------------------+
                      /                  |                  \
                     /                   |                   \
       (US-West Client)           (US-East Client)           (EU-West Client)
                   /                     |                     \
                  v                      v                      v
        +---------------+      +---------------+      +---------------+
        | US-West App   |      | US-East App   |      | EU-West App   |
        +---------------+      +---------------+      +---------------+
         /            \         /            \         /            \
        v              v       v              v       v              v
  +-----------+  +-----------+-----------+  +-----------+  +-----------+
  | Local     |  | Local     | Primary   |  | Local     |  | Local     |
  | Cache     |  | DB        | DB        |  | Cache     |  | DB        |
  | (Redis)   |  | Replica   | (Postgres)|  | (Redis)   |  | Replica   |
  +-----------+  +-----------+-----------+  +-----------+  +-----------+
                       ^             |             ^
                       |             |             |
                       +-------------+-------------+
                         Asynchronous Replication
```

---

## 2. Distributed Systems & CAP Theorem Analysis

According to the **CAP Theorem**, a distributed data store can simultaneously provide at most two of the following three guarantees:
- **C**onsistency: Every read receives the most recent write or an error.
- **A**vailability: Every non-failing node returns a non-error response, without the guarantee that it contains the most recent write.
- **P**artition Tolerance: The system continues to operate despite an arbitrary number of messages being dropped or delayed by the network between nodes.

In a physical wide-area network (WAN), partitions are inevitable ($P$). Therefore, the system must choose between Consistency ($C$) and Availability ($A$).

### A. The Write Path: Prioritizing Consistency (CP System)
In a URL Shortener or Content Resolver, preventing key collision (e.g., two different clients creating different destinations for the same short code) is critical. 
- **Implementation**: All write operations (`POST /urls`), regardless of which regional endpoint they hit, are forwarded by the Global Router directly to the **US-East App Server** which executes writes exclusively on the **Primary Database**.
- **CAP Choice**: **CP**. If a network partition isolates the write region (`us-east`) or if the primary database crashes, write requests are rejected (`503 Service Unavailable`). We sacrifice write availability to guarantee strict global consistency (preventing collision/duplicate registrations).

### B. The Read Path: Prioritizing Availability (AP System)
Content delivery networks and URL redirection require extremely low latency and high uptime. 
- **Implementation**: Read requests (`GET /:code`) are served by local regional nodes using local Redis caches and PostgreSQL read replicas.
- **CAP Choice**: **AP**. If a network partition cuts off a region from the Primary database, or if a local cache fails, the region continues to serve reads. If a regional node goes down entirely, the Router redirects traffic to the next closest region. 

### C. Eventual Consistency & Replication Lag
Because writes go to `us-east` and reads go to local replicas, we use **Asynchronous Replication** to replicate data from the primary database to read replicas. 
- **Latency Cost**: Synchronous replication across regions would stall write latency (requiring multi-millisecond cross-Atlantic round-trips).
- **Eventual Consistency**: Replicas are eventually consistent. In our simulation:
  - US-East replicates to **US-West** with a **1.5s lag**.
  - US-East replicates to **EU-West** with a **3.0s lag**.
- **Read-Your-Own-Writes Anomaly**: If a client in Ireland (`eu-west`) creates a short URL, it writes to `us-east` immediately. If the client queries it in `eu-west` within 3 seconds, they will get a cache/replica miss. 
- **Mitigation/Fallback**: In case of a local replica miss, the Go app server performs a **Primary DB Fallback** read directly from the `us-east` primary database, backfilling the local cache/replica immediately. This guarantees monotonic read consistency at the expense of higher latency on the first read.

---

## 3. Fault-Tolerance & Failover Mechanics

The Go app server and Global Router employ multi-tiered failover checks to ensure high service availability:

### Tier 1: Local Cache Failover (Cache Down)
- **Normal state**: Read check hits Redis cache (`X-Source: Cache`). Latency < 2ms.
- **Failure state**: Redis cache goes down (simulated or crashed).
- **Failover response**: App server catches the error, bypasses the cache, and queries the local Postgres DB replica. It marks the response `X-Source: Local Replica DB`. Read availability is preserved, though database load and latency increase slightly.

### Tier 2: Replica DB Failover (Replica Down)
- **Normal state**: Cache miss leads to Local DB replica query.
- **Failure state**: Local DB replica goes down.
- **Failover response**: App server bypasses the replica and executes a cross-region query directly to the **Primary DB** in US-East. It marks the response `X-Source: Primary DB Fallback` and `X-Database-Fallback: true`.
- **Latency impact**: Latency increases (e.g. EU-West client experiences ~110ms cross-region latency), but availability is preserved. The app server backfills the local Redis cache once the query returns, shield-protecting subsequent requests.

### Tier 3: Regional Failure (App Server Down)
- **Failure state**: An entire regional App server crashes or is partitioned.
- **Failover response**: The Global Router's background health checker identifies the server is unreachable. When a client requests that region, the router overrides the geographic mapping and redirects the request to the next closest healthy region:
  - `us-west` fails over to `us-east`, then `eu-west`.
  - `eu-west` fails over to `us-east`, then `us-west`.
  - `us-east` fails over to `us-west`, then `eu-west`.
- **Response Headers**: The router injects `X-Failover-Triggered: true`, `X-Original-Region: us-west`, and `X-Served-By-Region: us-east`.

---

## 4. Simulated Network Latency Matrix

To model real-world WAN latency, the Global Router injects artificial sleep times on proxy paths:

| Client Location | Target: US-East | Target: US-West | Target: EU-West |
| :--- | :--- | :--- | :--- |
| **US-East** (Virginia) | 15 ms | 75 ms | 110 ms |
| **US-West** (Oregon) | 75 ms | 10 ms | 180 ms |
| **EU-West** (Ireland) | 110 ms | 180 ms | 12 ms |

---

## 5. Active-Passive vs Active-Active Analysis

In designing a multi-region system, a primary decision is whether to run an **Active-Active** or **Active-Passive** architecture for both the compute (App Servers) and storage (Databases) layers.

### A. Compute Layer: Active-Active App Servers
We chose an **Active-Active** configuration for our application compute servers. All three regional app instances are online, actively serving reads, checking local caches, and receiving write requests.
- **Trade-off**: Requires a Global Router (GSLB) that can intelligently parse the client's origin and route to the nearest compute node, keeping local reads extremely fast.
- **Benefit**: If one region fails, another is already running and warm. Rerouting traffic takes less than 2 seconds (Router health check poll window) without spin-up delays.

### B. Database Layer: Active-Passive (Single-Leader) Replication
We chose an **Active-Passive (Read Replicas)** configuration for the storage layer. US-East houses the single active primary (leader) database, while US-West and EU-West house passive streaming read-replicas (standby nodes for write, active for reads).
- **Why not Active-Active (Multi-Leader) Database?**
  1. *Write Conflict Resolution*: In a multi-leader setup, writing identical short codes in both EU and US simultaneously results in race conditions and split-brain conflicts, requiring complex resolution engines (e.g., CRDTs or last-write-wins).
  2. *Distributed Locking Overhead*: Assuring global uniqueness of short codes requires cross-region consensus protocols (like Paxos or Raft), which stalls write latency from ~15ms to 200ms+ due to round-trip WAN waits.
- **Trade-offs Made**:
  - **Write Availability sacrificed for Consistency (CP)**: If US-East goes down, we cannot accept writes.
  - **Read Latency optimized for Availability (AP)**: Reads are served locally from replicas with sub-30ms p95 latency.

---

## 6. Performance Benchmarks

| Metric | Target | Simulated/Measured Value | Notes |
| :--- | :--- | :--- | :--- |
| **p95 Read Latency (Cache Hit)** | < 100ms | **10 - 20 ms** | Local Redis cache check + local client-region network hop. |
| **p95 Read Latency (Replica Hit)** | < 100ms | **15 - 30 ms** | Local Postgres replica check + local network hop. |
| **Replication Lag (US-West)** | - | **1.5 seconds** | Simulated eventual consistency replication lag. |
| **Replication Lag (EU-West)** | - | **3.0 seconds** | Simulated cross-Atlantic replication lag. |
| **Failover Reroute Time (Compute)** | < 10s | **< 2.0 seconds** | Detected by active background health checking in Global Router. |
| **Local Cache/Replica Fallback** | Instant | **0 seconds** | Code execution falls back instantly inline with the client request. |

---

## 7. Chaos Test Log

The system was subjected to simulated infrastructure chaos. Telemetry and failover logs are documented below:

### Test Case 1: Regional Redis Cache Outage
- **Action**: Clicked `KILL` on **US-West Redis Cache**.
- **Simulated Event**: Local Redis cache container crashes or network partition cuts off the app from Redis.
- **Measured Response**: 
  - Subsequent read request to `us-west` returned HTTP `302 Found`.
  - Served by: `US-WEST`, Source: `Local Replica DB`.
  - Latency: Increased from **10ms** to **12ms** (DB overhead).
- **Recovery**: Automatic. App server handles cache connection errors inline and falls back immediately without crash. Once Redis is restored, the cache is automatically backfilled on the next read.

### Test Case 2: Regional Replica DB Outage
- **Action**: Clicked `KILL` on **US-West Postgres Replica**.
- **Simulated Event**: Replica DB storage crashes or replication pipeline breaks.
- **Measured Response**:
  - Read request to `us-west` resulted in cache miss, then replica DB query failed.
  - Inline code caught the error and routed query directly to the Primary DB in `us-east`.
  - Served by: `US-WEST`, Source: `Primary DB Fallback`, `X-Database-Fallback: true`.
  - Latency: Increased to **82ms** due to cross-region WAN network call.
- **Recovery**: Automatic. Local cache was backfilled with the returned URL. Subsequent requests were served from the cache (10ms) without hitting the broken database replica.

### Test Case 3: Complete Regional Compute Outage
- **Action**: Clicked `KILL` on **US-West App Server** (entire region down).
- **Simulated Event**: Region-wide power outage or container crash.
- **Measured Response**:
  - Global Router background health checker polled `us-west/health`, timed out, and marked it **Healthy=false** in the health table.
  - Detection/Failover Time: **1.2 seconds**.
  - Client request sent with `X-Client-Region: us-west` was intercepted by the router and forwarded to `us-east` (nearest backup).
  - Served by: `US-EAST`, Original Region: `US-WEST`, `X-Failover-Triggered: true`.
  - Latency: **75ms** (client-to-east latency).
- **Recovery**: Automatic. Once the US-West App Server was restarted, the Router health checker detected it within 2 seconds and automatically restored normal geographical affinity.

### Test Case 4: Primary Write Region Outage
- **Action**: Clicked `KILL` on **US-East Primary DB**.
- **Simulated Event**: Primary PostgreSQL database crashes.
- **Measured Response**:
  - Write request (`POST /urls`) returned `503 Service Unavailable`.
  - Read requests (`GET /:code`) from `us-west` and `eu-west` succeeded (served by local caches and replicas).
  - **Consistency Guarantee**: Zero data loss for existing records. Eventual consistency of replicated data was maintained.

