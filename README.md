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
### TODO::

---
## **Setup Instructions**
---
### TODO::

---
## Domains, Entities
---
**Domain entity:** `LogMonitor` — `monitor.go`

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

**Domain entity:** `SlidingWindow` — `sliding_window.go`
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

**Behaviour:**

- `Record(entry)` — called for every log line. Appends the request's timestamp to the global list and the IP's list. 
  - If the status code is ≥ 400, also appends to that IP's error list. 
  - After every append it **evicts** entries older than the window cutoff from the front of the list.

- `GetSnapshot(ip)` — returns a `point-in-time` count of how many entries are in each relevant list right now.
  - Also evicts stale entries before counting, so the count is always accurate to the current moment.

- `ActiveIPs()` — returns the set of IPs that have at least one entry in `ipWindows`, regardless of whether those entries are stale. 
  - evaluation loop uses this to know which IPs to check.
