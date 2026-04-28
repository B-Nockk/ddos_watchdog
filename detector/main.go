package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Log struct {
		Path string `yaml:"path"`
	} `yaml:"log"`

	Window struct {
		Seconds int `yaml:"seconds"`
	} `yaml:"window"`

	Baseline struct {
		WindowMinutes      int `yaml:"window_minutes"`
		RecalcIntervalSecs int `yaml:"recalc_interval_secs"`
	} `yaml:"baseline"`

	Detector struct {
		ZThreshold      float64 `yaml:"z_threshold"`
		RateMultiplier  float64 `yaml:"rate_multiplier"`
		ErrorMultiplier float64 `yaml:"error_multiplier"`

		Allowlist []string `yaml:"allowlist"`
	} `yaml:"detector"`

	Blocker struct {
		IPSetName       string `yaml:"ipset_name"`
		DurationMinutes []int  `yaml:"duration_minutes"`
	} `yaml:"blocker"`

	Unbanner struct {
		CheckIntervalSecs int `yaml:"check_interval_secs"`
	} `yaml:"unbanner"`

	Notifier struct {
		WebhookURL   string `yaml:"webhook_url"`
		AuditLogPath string `yaml:"audit_log_path"`
	} `yaml:"notifier"`

	Dashboard struct {
		Port int `yaml:"port"`
	} `yaml:"dashboard"`
}

func (cfg *Config) validate() error {
	if cfg.Log.Path == "" {
		return fmt.Errorf("log.path is required")
	}
	if cfg.Notifier.AuditLogPath == "" {
		return fmt.Errorf("notifier.audit_log_path is required")
	}
	if cfg.Dashboard.Port == 0 {
		return fmt.Errorf("dashboard.port is required")
	}
	if cfg.Blocker.IPSetName == "" {
		return fmt.Errorf("blocker.ipset_name is required")
	}
	return nil
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}

	if webhookURL := os.Getenv("SLACK_WEBHOOK_URL"); webhookURL != "" {
		cfg.Notifier.WebhookURL = webhookURL
	}
	if logPath := os.Getenv("LOG_PATH"); logPath != "" {
		cfg.Log.Path = logPath
	}
	if auditPath := os.Getenv("AUDIT_LOG_PATH"); auditPath != "" {
		cfg.Notifier.AuditLogPath = auditPath
	}
	if portStr := os.Getenv("DAEMON_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			cfg.Dashboard.Port = port
		}
	}

	if extra := os.Getenv("ALLOWLIST"); extra != "" {
		for _, e := range strings.Split(extra, ",") {
			if trimmed := strings.TrimSpace(e); trimmed != "" {
				cfg.Detector.Allowlist = append(cfg.Detector.Allowlist, trimmed)
			}
		}
	}

	return &cfg, nil
}

func main() {
	cfg, err := loadConfig("config.yml")
	if err != nil {
		log.Fatalf("[main] config load failed: %v", err)
	}

	if err := cfg.validate(); err != nil {
		log.Fatalf("config invalid: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	monitor, err := NewLogMonitor(cfg.Log.Path)
	if err != nil {
		log.Fatalf("[main] monitor init failed: %v", err)
	}

	window := NewSlidingWindow(cfg.Window.Seconds)
	baseline := NewBaselineEngine(cfg.Baseline.WindowMinutes, cfg.Baseline.RecalcIntervalSecs)
	blocker := NewBlocker(cfg.Blocker.IPSetName, cfg.Blocker.DurationMinutes)
	notifier := NewNotifier(cfg.Notifier.WebhookURL, cfg.Notifier.AuditLogPath)

	baseline.SetNotifier(notifier)

	allowlist := make(map[string]struct{}, len(cfg.Detector.Allowlist))
	for _, ip := range cfg.Detector.Allowlist {
		allowlist[strings.TrimSpace(ip)] = struct{}{}
	}
	log.Printf("[main] allowlist: %v", cfg.Detector.Allowlist)

	detector := NewAnomalyDetector(cfg.Detector.ZThreshold, cfg.Detector.RateMultiplier, cfg.Detector.ErrorMultiplier, blocker)

	unbanner := NewUnbanner(blocker, notifier, cfg.Unbanner.CheckIntervalSecs)
	dash := NewDashboard(blocker, window, baseline, cfg.Dashboard.Port)

	var entryCount atomic.Int64

	monitor.OnEntry(func(le LogEntry) {

		if _, ok := allowlist[le.SourceIP]; ok {
			return
		}
		window.Record(le)
		entryCount.Add(1)
	})

	detector.OnAnomaly(func(event AnomalyEvent) {
		if event.Kind == KindGlobal {
			notifier.SendGlobalAlert(event)
			return
		}

		if _, ok := allowlist[event.IP]; ok {
			log.Printf("[main] allowlist: skipping ban for %s", event.IP)
			return
		}

		record := blocker.Ban(event.IP)
		notifier.SendBanAlert(event, *record)
	})

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case ts := <-ticker.C:
				count := int(entryCount.Swap(0))
				baseline.RecordTick(count, ts)

				base := baseline.GetBaseline()

				if !base.Ready {
					log.Printf("[main] baseline warming up (%d samples)...", base.SampleCount)
					continue
				}

				activeIPs := window.ActiveIPs()

				for _, ip := range activeIPs {

					if _, ok := allowlist[ip]; ok {
						continue
					}
					snap := window.GetSnapshot(ip)
					detector.Evaluate(snap, base)
				}

				if len(activeIPs) > 0 {
					globalSnap := window.GetSnapshot(activeIPs[0])
					globalSnap.IP = ""
					detector.Evaluate(globalSnap, base)
				}
			}
		}
	}()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		unbanner.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		dash.Start(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := monitor.Start(ctx); err != nil {
			log.Printf("[main] monitor exited: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("[main] shutdown signal received — waiting for goroutines")
	wg.Wait()
	log.Printf("[main] clean shutdown complete")
}
