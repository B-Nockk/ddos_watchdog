package main

import (
	"context"
	"log"
	"time"
)

type Unbanner struct {
	blocker       *Blocker
	notifier      *Notifier
	checkInterval time.Duration
}

func (u *Unbanner) checkAndRelease(ip string, record BanRecord) {
	if record.UnbanAt == nil {
		return // permanent ban — never release
	}

	if time.Now().Before(*record.UnbanAt) {
		return // ban still active
	}

	u.blocker.Release(ip)
	log.Printf(
		"[unbanner] released %s after %d offense(s)",
		ip,
		record.OffenseCount,
	)

	if u.notifier != nil {
		u.notifier.SendUnbanAlert(ip, record)
	}
}

func (u *Unbanner) sweep() {
	banned := u.blocker.GetBanned()
	for ip, record := range banned {
		u.checkAndRelease(ip, record)
	}
}

func (u *Unbanner) Run(ctx context.Context) {
	ticker := time.NewTicker(u.checkInterval)
	defer ticker.Stop()

	log.Printf("[unbanner] started, checking every %s", u.checkInterval)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[unbanner] shutting down")
			return

		case <-ticker.C:
			u.sweep()
		}
	}
}

func NewUnbanner(blocker *Blocker, notifier *Notifier, checkIntervalSecs int) *Unbanner {
	return &Unbanner{
		blocker:       blocker,
		notifier:      notifier,
		checkInterval: time.Duration(checkIntervalSecs) * time.Second,
	}
}
