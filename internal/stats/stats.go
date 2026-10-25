// Package stats implements the pure measurement math: loss, RTT summary,
// jitter and the simplified E-model MOS (SPEC §7).
package stats

import (
	"math"

	"netdiag/internal/probe"
)

// MinJitterPairs is the number of usable RTT pairs below which jitter is
// unknown.
const MinJitterPairs = 2

// LossPct is failed / (ok + failed) in percent; skipped samples are not
// part of either. 0 when nothing was sent.
func LossPct(ok, failed int) float64 {
	if ok+failed == 0 {
		return 0
	}
	return 100 * float64(failed) / float64(ok+failed)
}

// Jitter is the mean of |RTT_i − RTT_{i−1}| over pairs of consecutive ok
// samples: the steady-state value of the RFC 3550 estimator, without its
// start-up bias toward 0 in short windows. A pair separated by a failed
// or skipped sample is not used. ok is false below MinJitterPairs pairs.
func Jitter(samples []probe.Sample) (jitterMs float64, ok bool) {
	sum, pairs := 0.0, 0
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		if a.Result != probe.OK || b.Result != probe.OK {
			continue
		}
		sum += math.Abs(b.RTTms() - a.RTTms())
		pairs++
	}
	if pairs < MinJitterPairs {
		return 0, false
	}
	return sum / float64(pairs), true
}

// StdDev is the population standard deviation (divide by n), as mtr shows.
func StdDev(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	ss := 0.0
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(v)))
}

// MOS is the simplified E-model (SPEC §7):
//
//	eff = avgRTT + 2·jitter + 10
//	R   = 93.2 − (eff/40 if eff < 160 else (eff − 120)/10) − 2.5·loss%
//	MOS = 1 + 0.035R + 7·10⁻⁶·R(R − 60)(100 − R), clamped to 1–4.5
//
// R is clamped to 0–100 first, where the polynomial is defined. Callers
// pass jitter 0 when it is unknown.
func MOS(avgRTTms, jitterMs, lossPct float64) float64 {
	eff := avgRTTms + 2*jitterMs + 10
	var r float64
	if eff < 160 {
		r = 93.2 - eff/40
	} else {
		r = 93.2 - (eff-120)/10
	}
	r -= 2.5 * lossPct
	r = min(max(r, 0), 100)
	mos := 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
	return min(max(mos, 1), 4.5)
}

// Summary is the per-target statistics row (SPEC §7). RTT values are in
// milliseconds and valid only when HasRTT is set.
type Summary struct {
	Sent    int // ok + failed; skipped samples are not sent
	OK      int
	Failed  int
	Skipped int
	LossPct float64

	HasRTT                         bool
	Last, Avg, Best, Worst, StdDev float64

	HasJitter bool // false: unknown, shown as "-", never exceeds a threshold
	Jitter    float64
}

// Summarize computes a Summary over samples ordered oldest first. Last is
// the RTT of the newest ok sample.
func Summarize(samples []probe.Sample) Summary {
	var s Summary
	rtts := make([]float64, 0, len(samples))
	for _, sm := range samples {
		switch sm.Result {
		case probe.OK:
			s.OK++
			rtts = append(rtts, sm.RTTms())
		case probe.Fail:
			s.Failed++
		case probe.Skipped:
			s.Skipped++
		}
	}
	s.Sent = s.OK + s.Failed
	s.LossPct = LossPct(s.OK, s.Failed)
	if len(rtts) == 0 {
		return s
	}
	s.HasRTT = true
	s.Best, s.Worst = rtts[0], rtts[0]
	sum := 0.0
	for _, v := range rtts {
		sum += v
		s.Best = min(s.Best, v)
		s.Worst = max(s.Worst, v)
	}
	s.Avg = sum / float64(len(rtts))
	s.Last = rtts[len(rtts)-1]
	s.StdDev = StdDev(rtts)
	s.Jitter, s.HasJitter = Jitter(samples)
	return s
}
