package main

import (
	"math"
	"sync"
	"time"
)

type AnomalyKind string
type TriggerKind string

const (
	KindPerIP  AnomalyKind = "per_ip"
	KindGlobal AnomalyKind = "global"
)

const (
	TriggerZScore         TriggerKind = "z_score"
	TriggerRateMultiplier TriggerKind = "rate_multiplier"
	TriggerErrorSurge     TriggerKind = "error_surge"
)

const alertCooldown = 30 * time.Second

const globalAlertCooldown = 2 * time.Minute

type AnomalyEvent struct {
	Kind           AnomalyKind
	IP             string
	CurrentRate    int
	BaselineMean   float64
	BaselineStddev float64
	ZScore         float64
	TriggeredBy    TriggerKind
	TimeStamp      time.Time
}

type AnomalyDetector struct {
	zThreshold      float64
	rateMultiplier  float64
	errorMultiplier float64
	handlers        []func(AnomalyEvent)

	blocker *Blocker

	mu              sync.Mutex
	lastAlertPerIP  map[string]time.Time
	lastGlobalAlert time.Time
}

func (ad *AnomalyDetector) OnAnomaly(fn func(AnomalyEvent)) {
	ad.handlers = append(ad.handlers, fn)
}

func (ad *AnomalyDetector) zScore(rate, mean, stddev float64) float64 {
	if stddev < 1e-9 {
		return 0
	}
	return (rate - mean) / stddev
}

func (ad *AnomalyDetector) isAnomalous(rate, mean, stddev float64) bool {
	return ad.zScore(rate, mean, stddev) > ad.zThreshold ||
		rate > ad.rateMultiplier*mean
}

func (ad *AnomalyDetector) checkErrorSurge(snap WindowSnapshot, base BaselineSnapshot) bool {
	if snap.IPRate == 0 {
		return false
	}

	errorFraction := float64(snap.ErrorRate) / float64(snap.IPRate)
	return float64(snap.ErrorRate) > ad.errorMultiplier*base.Mean && errorFraction > 0.5
}

func (ad *AnomalyDetector) canAlert(ip string) bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()

	now := time.Now()
	if last, ok := ad.lastAlertPerIP[ip]; ok {
		if now.Sub(last) < alertCooldown {
			return false
		}
	}
	ad.lastAlertPerIP[ip] = now
	return true
}

func (ad *AnomalyDetector) canGlobalAlert() bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()

	now := time.Now()
	if now.Sub(ad.lastGlobalAlert) < globalAlertCooldown {
		return false
	}
	ad.lastGlobalAlert = now
	return true
}

func (ad *AnomalyDetector) emit(event AnomalyEvent) {
	event.ZScore = math.Round(event.ZScore*100) / 100
	for _, h := range ad.handlers {
		h(event)
	}
}

func (ad *AnomalyDetector) Evaluate(snap WindowSnapshot, base BaselineSnapshot) {
	now := time.Now()

	if snap.IP != "" && snap.IPRate > 0 {

		if ad.blocker != nil && ad.blocker.IsBanned(snap.IP) {
			return
		}

		z := ad.zScore(float64(snap.IPRate), base.Mean, base.Stddev)

		switch {
		case ad.checkErrorSurge(snap, base):
			if ad.canAlert(snap.IP) {
				ad.emit(AnomalyEvent{
					Kind:           KindPerIP,
					IP:             snap.IP,
					CurrentRate:    snap.ErrorRate,
					BaselineMean:   base.Mean,
					BaselineStddev: base.Stddev,
					ZScore:         z,
					TriggeredBy:    TriggerErrorSurge,
					TimeStamp:      now,
				})
			}

		case z > ad.zThreshold:
			if ad.canAlert(snap.IP) {
				ad.emit(AnomalyEvent{
					Kind:           KindPerIP,
					IP:             snap.IP,
					CurrentRate:    snap.IPRate,
					BaselineMean:   base.Mean,
					BaselineStddev: base.Stddev,
					ZScore:         z,
					TriggeredBy:    TriggerZScore,
					TimeStamp:      now,
				})
			}

		case float64(snap.IPRate) > ad.rateMultiplier*base.Mean:
			if ad.canAlert(snap.IP) {
				ad.emit(AnomalyEvent{
					Kind:           KindPerIP,
					IP:             snap.IP,
					CurrentRate:    snap.IPRate,
					BaselineMean:   base.Mean,
					BaselineStddev: base.Stddev,
					ZScore:         z,
					TriggeredBy:    TriggerRateMultiplier,
					TimeStamp:      now,
				})
			}
		}
	}

	if snap.IP == "" && snap.GlobalRate > 0 {
		z := ad.zScore(float64(snap.GlobalRate), base.Mean, base.Stddev)
		switch {
		case z > ad.zThreshold:
			if ad.canGlobalAlert() {
				ad.emit(AnomalyEvent{
					Kind:           KindGlobal,
					IP:             "",
					CurrentRate:    snap.GlobalRate,
					BaselineMean:   base.Mean,
					BaselineStddev: base.Stddev,
					ZScore:         z,
					TriggeredBy:    TriggerZScore,
					TimeStamp:      now,
				})
			}

		case float64(snap.GlobalRate) > ad.rateMultiplier*base.Mean:
			if ad.canGlobalAlert() {
				ad.emit(AnomalyEvent{
					Kind:           KindGlobal,
					IP:             "",
					CurrentRate:    snap.GlobalRate,
					BaselineMean:   base.Mean,
					BaselineStddev: base.Stddev,
					ZScore:         z,
					TriggeredBy:    TriggerRateMultiplier,
					TimeStamp:      now,
				})
			}
		}
	}
}

func NewAnomalyDetector(zThreshold, rateMultiplier, errorMultiplier float64, blocker *Blocker) *AnomalyDetector {
	return &AnomalyDetector{
		zThreshold:      zThreshold,
		rateMultiplier:  rateMultiplier,
		errorMultiplier: errorMultiplier,
		blocker:         blocker,
		lastAlertPerIP:  make(map[string]time.Time),
	}
}
