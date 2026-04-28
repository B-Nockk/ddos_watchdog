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
