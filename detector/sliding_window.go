package main

import (
	"container/list"
	"sync"
	"time"
)

type WindowSnapshot struct {
	IP         string
	IPRate     int
	GlobalRate int
	ErrorRate  int
}

type entry struct {
	ts time.Time
}

type SlidingWindow struct {
	windowSeconds  int
	ipWindows      map[string]*list.List
	globalWindow   *list.List
	ipErrorWindows map[string]*list.List
	mu             sync.RWMutex
}

func NewSlidingWindow(windowSeconds int) *SlidingWindow {
	return &SlidingWindow{
		windowSeconds:  windowSeconds,
		ipWindows:      make(map[string]*list.List),
		globalWindow:   list.New(),
		ipErrorWindows: make(map[string]*list.List),
	}
}

func (sw *SlidingWindow) evict(l *list.List, now time.Time) {
	cutoff := now.Add(-time.Duration(sw.windowSeconds) * time.Second)

	for {
		front := l.Front()
		if front == nil {
			break
		}

		if front.Value.(entry).ts.Before(cutoff) {
			l.Remove(front)
		} else {
			break
		}
	}
}

func (sw *SlidingWindow) Record(e LogEntry) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	now := e.Timestamp

	sw.globalWindow.PushBack(entry{ts: now})
	sw.evict(sw.globalWindow, now)

	if _, ok := sw.ipWindows[e.SourceIP]; !ok {
		sw.ipWindows[e.SourceIP] = list.New()
	}

	sw.ipWindows[e.SourceIP].PushBack(entry{ts: now})
	sw.evict(sw.ipWindows[e.SourceIP], now)

	if e.Status >= 400 {
		if _, ok := sw.ipErrorWindows[e.SourceIP]; !ok {
			sw.ipErrorWindows[e.SourceIP] = list.New()
		}

		sw.ipErrorWindows[e.SourceIP].PushBack(entry{ts: now})
		sw.evict(sw.ipErrorWindows[e.SourceIP], now)
	}
}

func (sw *SlidingWindow) GetSnapshot(ip string) WindowSnapshot {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	now := time.Now()
	ipRate := 0

	if l, ok := sw.ipWindows[ip]; ok {
		sw.evict(l, now)
		ipRate = l.Len()
	}

	errorRate := 0
	if l, ok := sw.ipErrorWindows[ip]; ok {
		sw.evict(l, now)
		errorRate = l.Len()
	}

	sw.evict(sw.globalWindow, now)
	globalRate := sw.globalWindow.Len()

	return WindowSnapshot{
		IP:         ip,
		IPRate:     ipRate,
		GlobalRate: globalRate,
		ErrorRate:  errorRate,
	}
}

func (sw *SlidingWindow) ActiveIPs() []string {
	sw.mu.RLock()
	defer sw.mu.RUnlock()

	ips := make([]string, 0, len(sw.ipWindows))

	for ip := range sw.ipWindows {
		ips = append(ips, ip)
	}

	return ips
}
