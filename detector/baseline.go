package main

import (
	"container/list"
	"math"
	"sync"
	"time"
)

const warmupPeriod = 2 * time.Minute

type BaselineSnapshot struct {
	Mean         float64
	Stddev       float64
	SampleCount  int
	HourSlotUsed bool
	Ready        bool
}

type tickEntry struct {
	ts    time.Time
	count int
}

type BaselineEngine struct {
	windowMinutes   int
	reCalcInterval  int
	perSecondCounts *list.List
	hourlySlots     map[int][]float64
	effectiveMean   float64
	effectiveStddev float64
	lastReCalc      time.Time
	startedAt       time.Time

	notifier *Notifier

	mu sync.Mutex
}

func (be *BaselineEngine) SetNotifier(n *Notifier) {
	be.mu.Lock()
	defer be.mu.Unlock()
	be.notifier = n
}

func (be *BaselineEngine) GetBaseline() BaselineSnapshot {
	be.mu.Lock()
	defer be.mu.Unlock()

	hour := time.Now().Hour()
	slots := be.hourlySlots[hour]

	ready := time.Since(be.startedAt) >= warmupPeriod

	return BaselineSnapshot{
		Mean:         be.effectiveMean,
		Stddev:       be.effectiveStddev,
		SampleCount:  be.perSecondCounts.Len(),
		HourSlotUsed: len(slots) >= 120,
		Ready:        ready,
	}
}

func (be *BaselineEngine) evictOldTicks(now time.Time) {
	cutoff := now.Add(-time.Duration(be.windowMinutes) * time.Minute)
	for {
		front := be.perSecondCounts.Front()
		if front == nil {
			break
		}
		if front.Value.(tickEntry).ts.Before(cutoff) {
			be.perSecondCounts.Remove(front)
		} else {
			break
		}
	}
}

func mean(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	return sum / float64(len(samples))
}

func stddev(samples []float64, m float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sumSq float64
	for _, v := range samples {
		d := v - m
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(samples)))
}

func (be *BaselineEngine) recalculate(now time.Time) {
	hour := now.Hour()
	slots := be.hourlySlots[hour]

	var samples []float64

	if len(slots) >= 120 {

		samples = slots
	} else {

		for el := be.perSecondCounts.Front(); el != nil; el = el.Next() {
			samples = append(samples, float64(el.Value.(tickEntry).count))
		}
	}

	if len(samples) == 0 {

		be.effectiveMean = math.Max(be.effectiveMean, 1.0)
		be.effectiveStddev = math.Max(be.effectiveStddev, 0.5)
		return
	}

	m := mean(samples)
	s := stddev(samples, m)

	be.effectiveMean = math.Max(m, 1.0)
	be.effectiveStddev = math.Max(s, 0.5)

	be.hourlySlots[hour] = append(slots, m)

	if be.notifier != nil {
		hourSlotUsed := len(be.hourlySlots[hour]) >= 120
		go be.notifier.SendBaselineRecalcAudit(be.effectiveMean, be.effectiveStddev, len(samples), hourSlotUsed)
	}
}

func (be *BaselineEngine) maybeReCalc(now time.Time) {
	if be.lastReCalc.IsZero() || now.Sub(be.lastReCalc) >= time.Duration(be.reCalcInterval)*time.Second {
		be.recalculate(now)
		be.lastReCalc = now
	}
}

func (be *BaselineEngine) RecordTick(count int, ts time.Time) {
	be.mu.Lock()
	defer be.mu.Unlock()

	be.perSecondCounts.PushBack(tickEntry{ts: ts, count: count})
	be.evictOldTicks(ts)
	be.maybeReCalc(ts)
}

func NewBaselineEngine(windowMinutes int, reCalcIntervalSecs int) *BaselineEngine {
	return &BaselineEngine{
		windowMinutes:   windowMinutes,
		reCalcInterval:  reCalcIntervalSecs,
		perSecondCounts: list.New(),
		hourlySlots:     make(map[int][]float64),
		effectiveMean:   1.0,
		effectiveStddev: 0.5,
		startedAt:       time.Now(),
	}
}
