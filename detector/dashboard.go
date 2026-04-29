package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sort"
	"time"
)

type ipMetric struct {
	IP        string `json:"ip"`
	IPRate    int    `json:"ip_rate"`
	ErrorRate int    `json:"error_rate"`
	Banned    bool   `json:"banned"`
}

type banEntry struct {
	IP              string  `json:"ip"`
	BannedAt        string  `json:"banned_at"`
	OffenseCount    int     `json:"offense_count"`
	DurationMinutes *int    `json:"duration_minutes"` // nil = permanent
	UnbanAt         *string `json:"unban_at"`         // nil = permanent
}

type offenseHistoryEntry struct {
	IP              string `json:"ip"`
	OffenseCount    int    `json:"offense_count"`
	LastSeen        string `json:"last_seen"`
	CurrentlyBanned bool   `json:"currently_banned"`
}

type metrics struct {
	Uptime         string                `json:"uptime"`
	GlobalRate     int                   `json:"global_rate"`
	BannedCount    int                   `json:"banned_count"`
	BannedIPs      []banEntry            `json:"banned_ips"`
	TopIPs         []ipMetric            `json:"top_ips"`
	OffenseHistory []offenseHistoryEntry `json:"offense_history"`
	BaselineMean   float64               `json:"baseline_mean"`
	BaselineStddev float64               `json:"baseline_stddev"`
	SampleCount    int                   `json:"sample_count"`
	HourSlotUsed   bool                  `json:"hour_slot_used"`
	BaselineReady  bool                  `json:"baseline_ready"`
	MemMB          float64               `json:"mem_mb"`
	NumGoroutine   int                   `json:"num_goroutine"`
}

type Dashboard struct {
	blocker   *Blocker
	window    *SlidingWindow
	baseline  *BaselineEngine
	store     *OffenseStore
	port      int
	startTime time.Time
}

func formatUptime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dh %dm %ds", h, m, s)
}

func (d *Dashboard) fetchOffenseHistory() []offenseHistoryEntry {
	rows, err := d.store.db.Query(`
		SELECT ip, count, last_seen
		FROM offense_history
		ORDER BY count DESC, last_seen DESC
		LIMIT 50
	`)
	if err != nil {
		log.Printf("[dashboard] offense history query: %v", err)
		return nil
	}
	defer rows.Close()

	banned := d.blocker.GetBanned()
	var entries []offenseHistoryEntry

	for rows.Next() {
		var ip, lastSeenStr string
		var count int

		if err := rows.Scan(&ip, &count, &lastSeenStr); err != nil {
			continue
		}

		// Parse and reformat the timestamp for display consistency.
		lastSeen, err := time.Parse(time.RFC3339, lastSeenStr)
		if err != nil {
			lastSeen = time.Time{}
		}

		_, currentlyBanned := banned[ip]

		entries = append(entries, offenseHistoryEntry{
			IP:              ip,
			OffenseCount:    count,
			LastSeen:        lastSeen.UTC().Format(time.RFC3339),
			CurrentlyBanned: currentlyBanned,
		})
	}

	return entries
}

func (d *Dashboard) buildMetrics() metrics {
	base := d.baseline.GetBaseline()
	banned := d.blocker.GetBanned()
	activeIPs := d.window.ActiveIPs()

	globalRate := 0
	if len(activeIPs) > 0 {
		globalRate = d.window.GetSnapshot(activeIPs[0]).GlobalRate
	}

	ipMetrics := make([]ipMetric, 0, len(activeIPs))
	for _, ip := range activeIPs {
		snap := d.window.GetSnapshot(ip)
		_, isBanned := banned[ip]

		ipMetrics = append(ipMetrics, ipMetric{
			IP:        ip,
			IPRate:    snap.IPRate,
			ErrorRate: snap.ErrorRate,
			Banned:    isBanned,
		})
	}

	// Sort descending by IPRate, take top 10.
	sort.Slice(ipMetrics, func(i, j int) bool {
		return ipMetrics[i].IPRate > ipMetrics[j].IPRate
	})
	if len(ipMetrics) > 10 {
		ipMetrics = ipMetrics[:10]
	}

	// Project BanRecords to banEntry.
	banEntries := make([]banEntry, 0, len(banned))
	for _, rec := range banned {
		e := banEntry{
			IP:              rec.IP,
			BannedAt:        rec.BannedAt.UTC().Format(time.RFC3339),
			OffenseCount:    rec.OffenseCount,
			DurationMinutes: rec.DurationMinutes,
		}

		if rec.UnbanAt != nil {
			s := rec.UnbanAt.UTC().Format(time.RFC3339)
			e.UnbanAt = &s
		}
		banEntries = append(banEntries, e)
	}

	// Sort banned IPs by BannedAt descending (most recent first).
	sort.Slice(banEntries, func(i, j int) bool {
		return banEntries[i].BannedAt > banEntries[j].BannedAt
	})

	// Memory stats.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return metrics{
		Uptime:         formatUptime(time.Since(d.startTime)),
		GlobalRate:     globalRate,
		BannedCount:    len(banned),
		BannedIPs:      banEntries,
		TopIPs:         ipMetrics,
		OffenseHistory: d.fetchOffenseHistory(),
		BaselineMean:   base.Mean,
		BaselineStddev: base.Stddev,
		SampleCount:    base.SampleCount,
		HourSlotUsed:   base.HourSlotUsed,
		BaselineReady:  base.Ready,
		MemMB:          float64(mem.Alloc) / 1024 / 1024,
		NumGoroutine:   runtime.NumGoroutine(),
	}
}

func (d *Dashboard) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	metrics := d.buildMetrics()
	if err := json.NewEncoder(w).Encode(metrics); err != nil {
		log.Printf("[dashboard] encode error: %v", err)
	}
}

func (d *Dashboard) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleUI)
	mux.HandleFunc("/metrics", d.handleMetrics)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", d.port),
		Handler: mux,
	}

	// Shut down cleanly when ctx is cancelled.
	go func() {
		<-ctx.Done()
		log.Printf("[dashboard] shutting down")
		_ = server.Shutdown(context.Background())
	}()

	log.Printf("[dashboard] listening on :%d", d.port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[dashboard] server error: %v", err)
	}
}

func NewDashboard(blocker *Blocker, window *SlidingWindow, baseline *BaselineEngine, store *OffenseStore, port int) *Dashboard {
	return &Dashboard{
		blocker:   blocker,
		window:    window,
		baseline:  baseline,
		store:     store,
		port:      port,
		startTime: time.Now(),
	}
}

// ==================================================================
// ui
// ==================================================================

func (d *Dashboard) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>DDoS Daemon — Dashboard</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: 'Courier New', monospace; background: #0d1117; color: #c9d1d9; padding: 24px; }
  h1 { color: #58a6ff; margin-bottom: 4px; font-size: 1.4rem; }
  .subtitle { color: #8b949e; font-size: 0.8rem; margin-bottom: 24px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 12px; margin-bottom: 24px; }
  .card { background: #161b22; border: 1px solid #30363d; border-radius: 6px; padding: 16px; }
  .card .label { font-size: 0.7rem; color: #8b949e; text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 6px; }
  .card .value { font-size: 1.6rem; color: #f0f6fc; font-weight: bold; }
  .card .value.danger { color: #f85149; }
  .card .value.warn  { color: #e3b341; }
  .card .value.ok    { color: #3fb950; }
  .section { margin-bottom: 24px; }
  .section h2 { font-size: 0.85rem; color: #8b949e; text-transform: uppercase; letter-spacing: 0.08em; margin-bottom: 10px; border-bottom: 1px solid #21262d; padding-bottom: 6px; }
  table { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
  th { text-align: left; color: #8b949e; font-weight: normal; padding: 6px 10px; border-bottom: 1px solid #21262d; }
  td { padding: 6px 10px; border-bottom: 1px solid #161b22; }
  tr:hover td { background: #1c2128; }
  .badge { display: inline-block; padding: 1px 7px; border-radius: 10px; font-size: 0.72rem; }
  .badge.banned    { background: #3d1f1f; color: #f85149; border: 1px solid #6e2a2a; }
  .badge.active    { background: #1a2f1a; color: #3fb950; border: 1px solid #2a5a2a; }
  .badge.permanent { background: #2d1f3d; color: #bc8cff; border: 1px solid #5a3a7a; }
  .badge.history   { background: #1f2a1f; color: #8b949e; border: 1px solid #2a3a2a; }
  .badge.warming   { background: #1f2a3d; color: #58a6ff; border: 1px solid #2a4a7a; }
  .offense-1 { color: #8b949e; }
  .offense-2 { color: #e3b341; }
  .offense-3 { color: #f0883e; }
  .offense-4 { color: #f85149; }
  .pulse { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: #3fb950; margin-right: 6px; animation: pulse 2s infinite; }
  .pulse.warn { background: #e3b341; }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:0.3} }
  .updated { font-size: 0.72rem; color: #484f58; margin-top: 16px; }
  .bar-wrap { display: flex; align-items: center; gap: 8px; }
  .bar { height: 6px; background: #58a6ff; border-radius: 3px; min-width: 2px; }
  .warmup-banner { background: #1f2a3d; border: 1px solid #2a4a7a; border-radius: 6px; padding: 10px 16px; margin-bottom: 16px; color: #58a6ff; font-size: 0.82rem; }
</style>
</head>
<body>
<h1><span class="pulse" id="pulse-dot"></span>DDoS Detection Daemon</h1>
<p class="subtitle" id="subtitle">connecting...</p>

<div id="warmup-banner" class="warmup-banner" style="display:none">
  ⏳ Baseline warming up — anomaly detection paused for 2 minutes while traffic data is collected.
</div>

<div class="grid" id="cards"></div>

<div class="section">
  <h2>Top IPs by Request Rate (last 60s)</h2>
  <table>
    <thead><tr><th>IP</th><th>Req/60s</th><th>Errors</th><th>Rate</th><th>Status</th></tr></thead>
    <tbody id="top-ips"></tbody>
  </table>
</div>

<div class="section">
  <h2>Currently Banned</h2>
  <table>
    <thead><tr><th>IP</th><th>Offenses</th><th>Banned At</th><th>Unban At</th><th>Duration</th></tr></thead>
    <tbody id="banned-ips"></tbody>
  </table>
</div>

<div class="section">
  <h2>Offense History — Persistent Record (last 50)</h2>
  <table>
    <thead><tr><th>IP</th><th>Total Offenses</th><th>Last Seen</th><th>Status</th></tr></thead>
    <tbody id="offense-history"></tbody>
  </table>
</div>

<p class="updated" id="updated"></p>

<script>
const MAX_BAR = 200;

function colorClass(rate, mean) {
  if (rate > mean * 5) return 'danger';
  if (rate > mean * 2) return 'warn';
  return 'ok';
}

function offenseClass(count) {
  if (count >= 4) return 'offense-4';
  if (count === 3) return 'offense-3';
  if (count === 2) return 'offense-2';
  return 'offense-1';
}

function fmt(ts) {
  if (!ts) return '—';
  return new Date(ts).toLocaleTimeString();
}

function fmtDate(ts) {
  if (!ts) return '—';
  const d = new Date(ts);
  return d.toLocaleDateString() + ' ' + d.toLocaleTimeString();
}

function render(m) {
  // Warmup banner
  const banner = document.getElementById('warmup-banner');
  const pulseDot = document.getElementById('pulse-dot');
  if (!m.baseline_ready) {
    banner.style.display = 'block';
    pulseDot.classList.add('warn');
  } else {
    banner.style.display = 'none';
    pulseDot.classList.remove('warn');
  }

  // Subtitle
  document.getElementById('subtitle').textContent =
    'Uptime: ' + m.uptime + ' | ' +
    'Baseline: ' + m.baseline_mean.toFixed(1) + ' req/s ±' + m.baseline_stddev.toFixed(1) +
    (m.hour_slot_used ? ' (hourly)' : ' (rolling)') +
    ' | Samples: ' + m.sample_count +
    (m.baseline_ready ? '' : ' | WARMING UP');

  // Stat cards
  const cards = [
    { label: 'Global Rate',   value: m.global_rate + ' req/60s', cls: colorClass(m.global_rate / 60, m.baseline_mean) },
    { label: 'Banned IPs',    value: m.banned_count,             cls: m.banned_count > 0 ? 'danger' : 'ok' },
    { label: 'Known IPs',     value: (m.offense_history || []).length, cls: '' },
    { label: 'Baseline Mean', value: m.baseline_mean.toFixed(1) + ' r/s', cls: '' },
    { label: 'Stddev',        value: '±' + m.baseline_stddev.toFixed(1), cls: '' },
    { label: 'Memory',        value: m.mem_mb.toFixed(1) + ' MB',        cls: '' },
    { label: 'Goroutines',    value: m.num_goroutine,                     cls: '' },
  ];
  document.getElementById('cards').innerHTML = cards.map(c =>
    '<div class="card"><div class="label">' + c.label + '</div>' +
    '<div class="value ' + c.cls + '">' + c.value + '</div></div>'
  ).join('');

  // Top IPs
  const topBody = document.getElementById('top-ips');
  if (!m.top_ips || m.top_ips.length === 0) {
    topBody.innerHTML = '<tr><td colspan="5" style="color:#484f58">No active IPs</td></tr>';
  } else {
    topBody.innerHTML = m.top_ips.map(ip => {
      const pct = Math.min(ip.ip_rate / MAX_BAR * 100, 100);
      const cls = colorClass(ip.ip_rate / 60, m.baseline_mean);
      const badge = ip.banned
        ? '<span class="badge banned">banned</span>'
        : '<span class="badge active">active</span>';
      return '<tr>' +
        '<td>' + ip.ip + '</td>' +
        '<td class="' + cls + '">' + ip.ip_rate + '</td>' +
        '<td>' + ip.error_rate + '</td>' +
        '<td><div class="bar-wrap"><div class="bar" style="width:' + pct + '%"></div></div></td>' +
        '<td>' + badge + '</td>' +
        '</tr>';
    }).join('');
  }

  // Currently banned
  const banBody = document.getElementById('banned-ips');
  if (!m.banned_ips || m.banned_ips.length === 0) {
    banBody.innerHTML = '<tr><td colspan="5" style="color:#484f58">No banned IPs</td></tr>';
  } else {
    banBody.innerHTML = m.banned_ips.map(b => {
      const durLabel = b.duration_minutes === null
        ? '<span class="badge permanent">permanent</span>'
        : b.duration_minutes + 'm';
      return '<tr>' +
        '<td>' + b.ip + '</td>' +
        '<td>' + b.offense_count + '</td>' +
        '<td>' + fmt(b.banned_at) + '</td>' +
        '<td>' + fmt(b.unban_at) + '</td>' +
        '<td>' + durLabel + '</td>' +
        '</tr>';
    }).join('');
  }

  // Offense history
  const histBody = document.getElementById('offense-history');
  if (!m.offense_history || m.offense_history.length === 0) {
    histBody.innerHTML = '<tr><td colspan="4" style="color:#484f58">No offense history recorded yet</td></tr>';
  } else {
    histBody.innerHTML = m.offense_history.map(h => {
      const statusBadge = h.currently_banned
        ? '<span class="badge banned">banned</span>'
        : '<span class="badge history">released</span>';
      const countClass = offenseClass(h.offense_count);
      return '<tr>' +
        '<td>' + h.ip + '</td>' +
        '<td class="' + countClass + '">' + h.offense_count + '</td>' +
        '<td>' + fmtDate(h.last_seen) + '</td>' +
        '<td>' + statusBadge + '</td>' +
        '</tr>';
    }).join('');
  }

  document.getElementById('updated').textContent =
    'Last updated: ' + new Date().toLocaleTimeString();
}

async function poll() {
  try {
    const resp = await fetch('/metrics');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();
    render(data);
  } catch (e) {
    document.getElementById('updated').textContent = 'Poll error: ' + e.message;
  }
}

poll();
setInterval(poll, 3000);
</script>
</body>
</html>`
