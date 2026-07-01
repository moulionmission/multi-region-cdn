# Operational Guide & System Architecture: APEX-CDN

This guide provides a comprehensive overview of **what** has been built, **how** it is implemented technically, and **how it works** under normal and failover operational paths.

---

## 1. What Has Been Done (Component Catalog)

The project directory (`/Users/b.v.ramana/Desktop/multi-region-cdn`) contains a full-stack, simulated multi-region Content Delivery Network (CDN) and URL mapping resolver. It is composed of three major layers:

### A. The Compute & Load Balancing Layer (Go)
1. **Global Router (`cmd/router/main.go`)**: 
   - Acts as a GSLB (Global Server Load Balancing) reverse proxy listening on port `8080`.
   - Proxies incoming client requests to regional app servers based on geographical headers.
   - Monitors regional server health via active background polling (`GET /health`) every 2 seconds.
   - Manages mock replication databases and coordinator services.
   - Serves the static dashboard web console.
2. **Regional App Server (`cmd/app/main.go`)**:
   - Runs independently in three simulated locations: `us-east` (port `8081`), `us-west` (port `8082`), and `eu-west` (port `8083`).
   - Resolves requests using local caches and read replicas, with a fallback connection to the primary database.
   - Exposes APIs to simulate/clear software failures (`POST /simulate/fail` and `/simulate/reset`).
3. **Local Mesh Runner (`main.go`)**:
   - Launches all four Go processes (3 Apps + 1 Router) concurrently in a single terminal session, setting environment variables, piping logs, and capturing Ctrl+C signals for a clean teardown.

### B. The Interactive Console Layer (HTML/CSS/JS)
1. **Console Interface (`web/index.html`)**: A sleek, black-mode dashboard featuring telemetry meters, visual layout elements, transaction tracers, and simulated kill toggles.
2. **Visual Flow Animator (`web/styles.css`)**: Implements CSS-based connection pathways. When a request is active, glowing colored dash patterns animate along SVG paths from the Client -> Router -> App -> Storage node.
3. **Client Orchestrator (`web/app.js`)**: Periodically queries regional `/health` and `/stats` endpoints, binds click events to component failure toggles, handles forms, and draws connection maps dynamically using viewport coordinate math.

### C. The Container & Deployment Layer
1. **Docker Config (`docker-compose.yml`, `docker/Dockerfile.app`, `docker/Dockerfile.router`)**: Builds light Alpine-based container images for Go and connects Postgres primary, Postgres replicas, Redis, regional app, and global proxy containers.
2. **Postgres Replication Scripts (`db/init-primary.sql`, `db/entrypoint-replica.sh`)**: Orchestrates automated PostgreSQL physical streaming replication by creating replication roles on the primary and executing `pg_basebackup` on standby replica startup.
3. **Kubernetes Configuration (`deployments/k8s-manifests.yaml`)**: Maps deployments, NodePorts, and ConfigMaps across four dedicated Namespaces (`us-east`, `us-west`, `eu-west`, `global-router`) to mirror multi-region compute/network partitions.

---

## 2. How It Is Done (Technical Implementation Details)

### A. Proximity Routing & GSLB
Client requests include an `X-Client-Region` header (e.g. `us-west`, `eu-west`, `us-east`). The Global Router interceptor parses this header:
- If a request is `POST /urls` (a write request), it ignores the origin header and proxies it directly to `us-east` (the designated Primary write datacenter), maintaining write consistency.
- If a request is `GET /:code` (a read/resolve request), it proxies it to the matching regional port.
- Proximity latencies are simulated by sleeping the proxy thread according to a geo-distance matrix:
  - Local region hop (e.g., client US-East -> US-East App): **15ms**
  - Coast-to-coast hop (e.g., client US-West -> US-East App): **75ms**
  - Transatlantic hop (e.g., client EU-West -> US-East App): **110ms**

### B. Multi-Tiered Read Resolution (Fallback Flow)
When a regional app receives a request to resolve a short code, it executes the following priority path:
1. **Step 1 (Cache)**: Checks the local Redis cache. If healthy and hit, returns redirection (`X-Source: Cache`).
2. **Step 2 (Local DB)**: On cache miss or cache failure, queries the local PostgreSQL replica. If hit, returns redirection (`X-Source: Local Replica DB`) and triggers an asynchronous backfill write to the local Redis cache.
3. **Step 3 (Primary DB Fallback)**: If the local DB replica query fails (connection down or replica crashed), it connects directly to the Primary DB in `us-east`. If hit, returns redirection (`X-Source: Primary DB Fallback`, `X-Database-Fallback: true`) and backfills the local cache and replica database to cover future hits.
4. **Step 4 (404/503)**: If the record doesn't exist, returns `404 Not Found`. If all datastores are unreachable (simulated outage), returns `503 Service Unavailable`.

### C. Eventual Consistency (Asynchronous Replication)
1. **Container Mode**: Postgres replicas continuously pull WAL (Write-Ahead Log) segments from the primary leader via TCP streaming replication. Replicas operate in hot-standby (read-only) mode.
2. **Mock Mode (Zero-Dependency)**: The Router acts as the mock DB coordinator. When `us-east` app writes a URL, it notifies the Router. The Router stores it in a primary map, then fires asynchronous Go timers (`time.Sleep`) to call `/api/mock/replica/sync` on regional app servers:
   - Synchronizes with `us-west` after **1.5 seconds**.
   - Synchronizes with `eu-west` after **3.0 seconds**.
   - This simulates WAN replication delay, allowing you to observe eventual consistency (replica lag) in real-time.

---

## 3. How It Works (Step-by-Step Scenario Analysis)

### Scenario A: Normal Write & Replication Flow
1. **Write Request**: A client in `EU-West` submits a request to shorten `https://wikipedia.org`.
2. **Forwarding**: The Global Router intercepts the request, notes it is a write (`POST /urls`), and routes it to the `US-East App Server`.
3. **Write Execution**: `US-East App` writes the record to the `Postgres Primary DB` (or mock primary map in the Router).
4. **Sync Loop**: 
   - The primary database registers the write.
   - An asynchronous replication loop sends WAL updates to `db-us-west` and `db-eu-west`.
   - In mock mode, the Router waits 1.5 seconds, then writes the record to `us-west` app's memory; it waits 3.0 seconds, then writes to `eu-west` app's memory.
5. **Eventual Read**: A client in Ireland (`eu-west`) queries the short code 4 seconds later. The read hits `EU-West App` -> `EU-West Redis Cache` (miss) -> `EU-West DB Replica` (hit). The URL resolves locally in **12ms**.

### Scenario B: Complete Compute Outage (Failover Flow)
1. **Outage**: The `US-West App Server` crashes (simulated by clicking `KILL` next to the US-West App Server).
2. **Health Check Detection**: The Global Router's background health checker attempts to call `http://localhost:8082/health`, fails to connect, and flags `us-west` as **Healthy = false**.
3. **Rerouting**: A client in California (`us-west`) requests URL resolution. The Global Router checks the health table, sees `us-west` is down, and looks up the proximity failover list:
   - For `us-west`, primary backup is `us-east`.
4. **Serving**: The router proxies the request to `US-East App Server` on port `8081`.
5. **Response**: The request is successfully resolved by the US-East server. The response header contains `X-Failover-Triggered: true`, `X-Served-By-Region: us-east`, and `X-Original-Region: us-west`.
6. **Recovery**: Once the developer clicks `RESTORE` on the dashboard, the app server boots, the Router detects a successful `/health` query (within 2 seconds), and normal geographic routing resumes.
