package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

const slackCooldown = 60 * time.Second

type Notifier struct {
	webhookURL   string
	auditLogPath string
	httpClient   *http.Client

	mu            sync.Mutex
	lastSlackSent map[string]time.Time
}

func (n *Notifier) canPostSlack(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := time.Now()
	if last, ok := n.lastSlackSent[key]; ok {
		if now.Sub(last) < slackCooldown {
			return false
		}
	}
	n.lastSlackSent[key] = now
	return true
}

func (n *Notifier) WriteAudit(action, ip, condition, rate, baseline, duration string) {
	line := fmt.Sprintf("[%s] %s %s | %s | %s | %s | %s\n",
		time.Now().UTC().Format(time.RFC3339),
		action,
		ip,
		condition,
		rate,
		baseline,
		duration,
	)

	f, err := os.OpenFile(n.auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[notifier] audit open error: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.WriteString(line); err != nil {
		log.Printf("[notifier] audit write error: %v", err)
	}
}

func (n *Notifier) postSlack(payload map[string]any) {
	if n.webhookURL == "" {
		return
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[notifier] slack marshal error: %v", err)
		return
	}

	resp, err := n.httpClient.Post(n.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notifier] slack post error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		log.Printf("[notifier] slack rate-limited (429) — will retry after cooldown")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("[notifier] slack returned non-2xx: %d", resp.StatusCode)
	}
}

func (n *Notifier) SendBanAlert(event AnomalyEvent, record BanRecord) {
	duration := formatDuration(record.DurationMinutes)

	n.WriteAudit(
		"BAN",
		record.IP,
		fmt.Sprintf("kind=%s trigger=%s", event.Kind, event.TriggeredBy),
		fmt.Sprintf("%d req/60s", event.CurrentRate),
		fmt.Sprintf("mean=%.1f stddev=%.1f z=%.2f", event.BaselineMean, event.BaselineStddev, event.ZScore),
		duration,
	)

	if !n.canPostSlack(record.IP) {
		log.Printf("[notifier] Slack cooldown active for %s — audit only", record.IP)
		return
	}

	var unbanInfo string
	if record.UnbanAt != nil {
		unbanInfo = fmt.Sprintf("Unban at: `%s`", record.UnbanAt.UTC().Format(time.RFC3339))
	} else {
		unbanInfo = "Unban at: `never (permanent)`"
	}

	text := fmt.Sprintf(
		":no_entry: *BANNED* `%s`\n"+
			">Kind: `%s` | Trigger: `%s`\n"+
			">Rate: `%d req/60s` | Mean: `%.1f` | Stddev: `%.1f` | Z: `%.2f`\n"+
			">Offense #%d | Duration: `%s`\n"+
			">%s\n"+
			">Banned at: `%s`",
		record.IP,
		event.Kind,
		event.TriggeredBy,
		event.CurrentRate,
		event.BaselineMean,
		event.BaselineStddev,
		event.ZScore,
		record.OffenseCount,
		duration,
		unbanInfo,
		record.BannedAt.UTC().Format(time.RFC3339),
	)

	n.postSlack(map[string]any{"text": text})
}

func (n *Notifier) SendUnbanAlert(ip string, record BanRecord) {
	duration := formatDuration(record.DurationMinutes)

	n.WriteAudit(
		"UNBAN",
		ip,
		fmt.Sprintf("offenses=%d", record.OffenseCount),
		"-",
		"-",
		duration,
	)

	n.mu.Lock()
	delete(n.lastSlackSent, ip)
	n.mu.Unlock()

	text := fmt.Sprintf(
		":white_check_mark: *UNBANNED* `%s`\n"+
			">Offense count was: `%d` | Ban duration was: `%s`\n"+
			">Released at: `%s`",
		ip,
		record.OffenseCount,
		duration,
		time.Now().UTC().Format(time.RFC3339),
	)

	n.postSlack(map[string]any{"text": text})
}

func (n *Notifier) SendGlobalAlert(event AnomalyEvent) {
	n.WriteAudit(
		"GLOBAL_ANOMALY",
		"-",
		fmt.Sprintf("trigger=%s", event.TriggeredBy),
		fmt.Sprintf("%d req/60s", event.CurrentRate),
		fmt.Sprintf("mean=%.1f stddev=%.1f z=%.2f", event.BaselineMean, event.BaselineStddev, event.ZScore),
		"-",
	)

	text := fmt.Sprintf(
		":warning: *GLOBAL ANOMALY DETECTED*\n"+
			">Trigger: `%s` | Global rate: `%d req/60s`\n"+
			">Mean: `%.1f` | Stddev: `%.1f` | Z: `%.2f`\n"+
			">Detected at: `%s`",
		event.TriggeredBy,
		event.CurrentRate,
		event.BaselineMean,
		event.BaselineStddev,
		event.ZScore,
		event.TimeStamp.UTC().Format(time.RFC3339),
	)

	n.postSlack(map[string]any{"text": text})
}

func (n *Notifier) SendBaselineRecalcAudit(mean, stddev float64, sampleCount int, hourSlotUsed bool) {
	source := "rolling"
	if hourSlotUsed {
		source = "hourly"
	}
	n.WriteAudit(
		"BASELINE_RECALC",
		"-",
		fmt.Sprintf("source=%s samples=%d", source, sampleCount),
		"-",
		fmt.Sprintf("mean=%.3f stddev=%.3f", mean, stddev),
		"-",
	)
}

func NewNotifier(webhookURL, auditLogPath string) *Notifier {
	return &Notifier{
		webhookURL:    webhookURL,
		auditLogPath:  auditLogPath,
		lastSlackSent: make(map[string]time.Time),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}
