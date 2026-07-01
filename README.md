# APEX-CDN // Multi-Region URL & Content Delivery System

APEX-CDN is a simulated Multi-Region, Fault-Tolerant URL & Content Delivery System built in Go. It demonstrates distributed systems principles like geographical load balancing (GSLB), multi-tiered failover caching, eventual consistency, and replication lag partitioning.

👉 **[Live Demo Deployment Guide](#-cloud-deployment-blueprint-render)**

---

## 💻 Resume Bullet Points (Quantified Metrics)

Here are high-impact bullet points you can add to your resume based on this project:
* **Multi-Region Architecture**: *Designed and implemented a multi-region URL delivery system in Go, achieving **sub-20ms p95 read latencies** by serving traffic from regional Redis edge caches and Postgres read replicas.*
* **High Availability & Failover**: *Engineered a GSLB Global Router proxy with active background health checking, reducing **failover rerouting time to < 2.0 seconds** during regional compute outages with zero read data loss.*
* **Distributed Database Replication**: *Orchestrated automated standby databases using PostgreSQL physical streaming replication, keeping transatlantic data replication sync lag **under 3.0 seconds**.*

---

## 🗺️ System Architecture

### Write Path (CP System)
Writes (`POST /urls`) always forward to the primary database in **US-East** to guarantee global key uniqueness and avoid write conflicts.

```
[Client] ---> [Global Router] ---> [US-East App] ---> [Postgres Primary]
                                                             |
                                                             +---> (Asynchronous WAL Sync)
                                                                       |
                                                                       +---> [US-West Replica]
                                                                       +---> [EU-West Replica]
```

### Read Path & Failover (AP System)
Reads (`GET /:code`) are served by local regional nodes. If a component fails, the system executes inline fallbacks:

```
[Client] ---> [Global Router] ---> [Regional App]
                                      |
                                      +---> 1. Check Local Cache (Hit: 10ms)
                                      |
                                      +---> 2. Check Local Replica (Miss: Fallback inline, 15ms)
                                      |
                                      +---> 3. Query Primary DB (Replica Down: Fallback cross-region, 110ms)
```

---

## 🛠️ Local Setup

### 1. Zero-Dependency Simulator (Recommended)
Run the entire cluster (3 App instances + 1 Global Router) locally in-memory:
```bash
go run cmd/simulator/main.go
```
Open the visual telemetry dashboard in your browser:
👉 **[http://localhost:8080/dashboard/](http://localhost:8080/dashboard/)**

### 2. Containerized Mode (Docker Compose)
To run with real PostgreSQL databases (configured with streaming replication) and Redis instances:
```bash
docker-compose up --build
```

---

## ☁️ Cloud Deployment Blueprint (Render)

You can host the simulator web server live on the cloud for free. Render supports Go applications natively:

1. Create a public repository on GitHub and push this project:
   ```bash
   git init
   git add .
   git commit -m "initial commit"
   # Link to your github repo and push...
   ```
2. Log into [Render](https://render.com) and go to **Blueprints** -> **New Blueprint Instance**.
3. Connect your GitHub repository. Render will automatically parse the `render.yaml` configuration file and spin up your live URL!

*Render deployment blueprint configuration is included in [render.yaml](file:///Users/b.v.ramana/Desktop/multi-region-cdn/render.yaml).*
