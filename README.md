# DDOS WatchDog

## **The daemon sits beside an app's network path.**

  - Nginx receives all HTTP traffic and forwards it to the app's docker instance. 
  - The daemon's relationship with nginx is through a shared log volume: 
    - nginx writes every request as a structured JSON line to `/var/log/nginx/hng-access.log`
    - that file lives on a named Docker volume called `HNG-nginx-logs` that both nginx and the daemon mount. 
    - The daemon mounts it read-only.

## **Why this matters architecturally:**

  - The daemon is completely out of band
  - it cannot slow down or block a request in flight. 
  - Detection and blocking happen asynchronously. 
  - **trade-off** - small detection lag (milliseconds to seconds)
  - **upside** - a bug or crash in the daemon cannot take down Nextcloud.

---
## **Language Choice & WHy**
---

### **Language:**

- **GO**

### **Why Go?**

- **Strong typing:** Go enforces type safety at compile time, reducing runtime errors and making large systems more reliable.  
- **Concurrency model:** Goroutines and channels provide lightweight, efficient concurrency — simpler and more predictable than Python’s threads or async/await.  
- **Performance:** Compiles to native machine code, giving near‑C speed while retaining simplicity. This makes it well‑suited for high‑volume log processing and anomaly detection.  
- **Cloud & DevOps alignment:** Go is widely used in cloud infrastructure.  
- **Deployment simplicity:** Produces static binaries with minimal dependencies, making containerization and deployment straightforward compared to languages that require heavier runtimes.  
- **Memory efficiency:** Goroutines consume far less memory than OS threads, allowing thousands of concurrent tasks without exhausting resources.  


---

## **Setup Instructions**

---

### A. **CI/CD via GitHub Actions (GHCR)**

- Workflow (`.github/workflows/deploy.yml`) builds and pushes the Docker image to **GitHub Container Registry (GHCR)**.  
- **Triggering:**  
  - From GitHub web UI (Actions tab → “Run workflow”).  
  - Or via CLI:  
    ```bash
    gh workflow run deploy.yml
    ```
- **Secrets required:**  
  - `GHCR_TOKEN` — PAT with `read:packages` and `write:packages`.  
  - `REGISTRY` — registry namespace (e.g., `ghcr.io/<username>`).  
  - `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY` — SSH credentials.  
  - `DEPLOY_PATH` — target path on server.  
  - `GCP_ENV_FILE` — environment variables for the service.  
- **Authentication:** Github's `Personal Access Token` is required to log in to GHCR before pulling.
- **Flexibility:** Works for GCP or any platform with SSH + Docker Compose.

---

### B. **Manual clone + build (cloud terminal)**

- If you don’t run the workflow, no image will exist in GHCR.  
- Alternative: clone the repo and build directly on your VM:  
  ```bash
  git clone https://github.com/<your-org>/<your-repo>.git
  cd <your-repo>
  docker compose up --build -d
  ```
- **Behavior:**  
  - `--build` ensures the service image is built from the included `Dockerfile`.  
  - Docker Compose automatically pulls referenced images (e.g., `kefaslungu/hng-nextcloud`) if missing.  
- Same as running locally, but executed inside the cloud machine.

---

### 3. **Local Development**

- You can run the detector stack on your own machine for testing.  
- Steps:  
  1. Clone the repo:  

  ```bash
     git clone https://github.com/<your-org>/<your-repo>.git
     cd <your-repo>
     ```

  2. Start services:  

  ```bash
     docker compose up --build -d
     ```

- **Behavior:**  
  - Builds the detector image locally.  
  - Pulls dependent images automatically.  
  - You can access the dashboard and logs on your local environment just like in production.  
- Useful for debugging, experimenting with config values, or validating changes before pushing to CI/CD.

---

### Key difference

- **Actions path:** centralized build → image pushed once, pulled anywhere (requires PAT).  
- **Manual path:** local build on VM → no registry needed, slower if VM resources are limited.  
- **Local dev:** same as manual build, but on your own machine for testing/debugging.


---

## **Accessing the Dashboard**

---

### A. **When deployed (cloud)**

- The Nginx reverse proxy exposes the detector dashboard at your configured domain.  
- **Endpoints:**  

  ```bash
  `https://ddos-watchdog.duckdns.org/ddos-dashboard/` → HTML dashboard UI.  
  `https://ddos-watchdog.duckdns.org/metrics` → JSON metrics feed.
  ```

- Nginx handles SSL termination (Let’s Encrypt certificates) and proxies requests to the daemon running on port `8080`.  
- Healthcheck endpoint:  

```bash
  `https://ddos-watchdog.duckdns.org/nginx-health` → returns `ok` if Nginx is healthy.
```

### 2. **When running locally**

- If you run `docker compose up --build -d` on your machine, the daemon is exposed directly without Nginx SSL.  
- **Endpoints:**  

```bash
`http://localhost:8080/` → HTML dashboard UI.  
`http://localhost:8080/metrics` → JSON metrics feed.  
```

- Since Nginx isn’t fronting the service locally, you don’t need HTTPS or domain setup — just hit `localhost:8080`.


---
---

## **Domains, Entities**

---
---

## **Domain entity:** `LogMonitor` — `monitor.go`

**Properties:**

- `logPath` — path to the nginx access log file
- `file` — the open file handle being tailed
- `lastInode` — inode number of the file when it was opened
- `handlers` — list of callback functions to invoke for each parsed entry

**Behaviour:**

- Opens the log file and **seeks to the end** on startup — it only cares about new traffic, not history
- Reads line by line in a tight loop; when no new line is available it sleeps 100ms and retries (this is a manual `tail -f`)
- Detects **log rotation** by comparing the current file's inode against `lastInode` — if they differ, the file was rotated, so it closes and reopens
- Parses each line from JSON into a `LogEntry` struct (source IP, timestamp, method, path, status code, response size)
- Calls every registered handler function with the parsed entry

---

## **Domain entity:** `SlidingWindow` — `sliding_window.go`

- Every parsed LogEntry needs to be counted in two dimensions simultaneously: 
  - globally (all traffic) &
  - per-IP.
- counts need to be time-aware 
  - a request from 90 seconds ago shouldn’t influence whether a current rate looks anomalous

**Properties:**

- `windowSeconds` — how far back the window extends (60 seconds)
- `globalWindow` — a linked list of timestamps for all requests
- `ipWindows` — a map from IP address to its own linked list of timestamps
- `ipErrorWindows` — a map from IP address to a linked list of timestamps for only 4xx/5xx responses
- `mu` — a read-write mutex protecting all three data structures


**How it works:**

  *example flow:*
  
      > IP A makes 3 requests, IP B makes 2,
      > global window has 5 entries, 
      > after 60s the oldest are evicted

- **Deque structure:**  
  - Each window (`globalWindow`, `ipWindows[ip]`, `ipErrorWindows[ip]`) is a linked list acting like a deque.  
  - New entries are always appended at the back.  
  - Old entries are evicted from the front, so the list always represents the last *N seconds* of activity.

- **Eviction logic:**  
  - On every insert or snapshot, the `evict()` function walks from the front of the list.  
  - It removes entries whose timestamp is older than `now - windowSeconds`.  
  - Because the front is always the oldest, eviction is efficient (O(1) per removal).

- **Global vs IP windows:**  
  - `globalWindow` tracks *all requests* across the system.  
  - `ipWindows[ip]` tracks requests from a single IP.  
  - `ipErrorWindows[ip]` tracks only error responses (status ≥ 400) from that IP.  
  - This separation lets you distinguish between **global anomalies** (system under load) and **per‑IP anomalies** (one client misbehaving).

- **Snapshots:**  
  - `GetSnapshot(ip)` evicts stale entries, then counts the length of each relevant list.  
  - Returns a `WindowSnapshot` with current rates for that IP, global traffic, and error traffic.  
  - Always reflects the *current moment* because eviction is applied before counting.

- **Concurrency:**  
  - A read‑write mutex (`mu`) protects all lists.  
  - Writers (`Record`) take a full lock, readers (`GetSnapshot`, `ActiveIPs`) take a read lock.  
  - Ensures thread‑safe updates even under high log volume.


**Behaviour:**

- `Record(entry)` — called for every log line. Appends the request's timestamp to the global list and the IP's list. 
  - If the status code is ≥ 400, also appends to that IP's error list. 
  - After every append it **evicts** entries older than the window cutoff from the front of the list.

- `GetSnapshot(ip)` — returns a `point-in-time` count of how many entries are in each relevant list right now.
  - Also evicts stale entries before counting, so the count is always accurate to the current moment.

- `ActiveIPs()` — returns the set of IPs that have at least one entry in `ipWindows`, regardless of whether those entries are stale. 
  - evaluation loop uses this to know which IPs to check.


---

## **Domain entity:** `BaselineEngine` — `baseline.go`

- Learns the volume boundary of normal traffic dynamically.  
- Avoids hardcoded thresholds by continuously updating from observed traffic.  
- Adjusts for time‑of‑day differences and long‑term patterns.

---

**Properties:**

- `windowMinutes` — rolling sample window size (30 minutes).  
- `reCalcInterval` — how often to recompute baseline (every 60 seconds).  
- `perSecondCounts` — linked list of tick entries, each holding timestamp + request count for one second.  
- `hourlySlots` — map from hour‑of‑day (0–23) to historical mean values, building a time‑of‑day profile.  
- `effectiveMean` / `effectiveStddev` — currently active baseline values used by detector.  
- `startedAt` — creation time, used to enforce warmup grace period (2 minutes).  
- `mu` — mutex protecting all structures.  

---

**How it works:**

*example flow:*

  ```txt
    > At 12:00:01, 120 requests observed → tickEntry added
    > At 12:00:02, 80 requests observed → tickEntry added
    > After 30 minutes, oldest ticks evicted
    > Every 60s, baseline recalculated from recent samples
    > If enough hourly data exists (≥120 samples for that hour), use that instead
  ```

- **Deque structure:**  
  - `perSecondCounts` is a linked list acting like a deque.  
  - New ticks are appended at the back.  
  - Old ticks beyond `windowMinutes` are evicted from the front.  

- **Recalculation intervals:**  
  - Every `reCalcInterval` seconds, `maybeReCalc` triggers `recalculate`.  
  - Chooses sample set: hourly slot if mature enough, otherwise rolling window.  
  - Updates `effectiveMean` and `effectiveStddev`.  

- **Floor values:**  
  - Mean never below 1.0, stddev never below 0.5.  
  - Prevents division‑by‑zero and avoids hypersensitivity during quiet periods.  

- **Warmup period:**  
  - First 2 minutes marked as not ready (`Ready=false`).  
  - Ensures baseline stabilizes before anomaly detection starts.  

---

**Behaviour:**

- `RecordTick(count, ts)` — called once per second. Appends tick, evicts old ticks, maybe recalculates.  
- `maybeReCalc(now)` — checks interval, triggers recalculation if due.  
- `recalculate(now)` — computes mean/stddev from chosen sample set, applies floor values, updates hourly slots.  
- `GetBaseline()` — returns snapshot of current baseline values, including whether hourly slots were used and whether warmup is complete.  
- `SetNotifier(n)` — attaches notifier for audit logging of recalculations.  

---
