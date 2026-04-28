package main

import (
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

type BanRecord struct {
	IP              string
	BannedAt        time.Time
	DurationMinutes *int
	OffenseCount    int
	UnbanAt         *time.Time
}

type Blocker struct {
	banned    map[string]*BanRecord
	durations []*int
	ipsetName string
	mu        sync.RWMutex
}

func formatDuration(d *int) string {
	if d == nil {
		return "permanent"
	}
	return fmt.Sprintf("%dm", *d)
}

func (b *Blocker) IsBanned(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.banned[ip]
	return ok
}

func (b *Blocker) IsPermanentlyBanned(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	rec, ok := b.banned[ip]
	if !ok {
		return false
	}
	return rec.UnbanAt == nil
}

func (b *Blocker) GetBanned() map[string]BanRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make(map[string]BanRecord, len(b.banned))
	for ip, rec := range b.banned {
		out[ip] = *rec
	}
	return out
}

func (b *Blocker) Release(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.run("ipset", "del", b.ipsetName, ip); err != nil {
		log.Printf("[blocker] ipset delete %s: %v", ip, err)
	}

	delete(b.banned, ip)
	log.Printf("[blocker] released %s", ip)
}

func (b *Blocker) run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func unbanAt(now time.Time, duration *int) *time.Time {
	if duration == nil {
		return nil
	}

	t := now.Add(time.Duration(*duration) * time.Minute)
	return &t
}

func (b *Blocker) setupIPSet() {
	if err := b.run(
		"ipset",
		"create",
		b.ipsetName,
		"hash:ip",
		"--exist",
	); err != nil {
		log.Printf("[blocker] ipset create warning: %v", err)
	}

	if err := b.run(
		"iptables", "-A", "INPUT",
		"-m", "set", "--match-set", b.ipsetName, "src",
		"-j", "DROP",
	); err != nil {
		log.Printf("[blocker] iptables rule warning: %v", err)
	}
}

func NewBlocker(IpsetName string, durationMinutes []int) *Blocker {
	ladder := make([]*int, len(durationMinutes)+1)
	for i, d := range durationMinutes {
		v := d
		ladder[i] = &v
	}
	ladder[len(durationMinutes)] = nil
	b := &Blocker{
		banned:    make(map[string]*BanRecord),
		durations: ladder,
		ipsetName: IpsetName,
	}

	b.setupIPSet()
	return b
}

func (b *Blocker) Ban(ip string) *BanRecord {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, alreadyBanned := b.banned[ip]

	if alreadyBanned && existing.UnbanAt == nil {
		log.Printf("[blocker] %s is already permanently banned — skipping", ip)
		return existing
	}

	offenseCount := 0
	if alreadyBanned {
		offenseCount = existing.OffenseCount
	}

	ladderIdx := offenseCount
	if ladderIdx >= len(b.durations) {
		ladderIdx = len(b.durations) - 1
	}

	duration := b.durations[ladderIdx]

	now := time.Now()
	record := &BanRecord{
		IP:              ip,
		BannedAt:        now,
		DurationMinutes: duration,
		OffenseCount:    offenseCount + 1,
		UnbanAt:         unbanAt(now, duration),
	}

	b.banned[ip] = record

	if err := b.run("ipset", "add", b.ipsetName, ip, "--exist"); err != nil {
		log.Printf("[blocker] ipset add %s: %v", ip, err)
	}

	log.Printf(
		"[blocker] banned %s offense=%d duration=%s",
		ip,
		record.OffenseCount,
		formatDuration(duration),
	)

	return record
}
