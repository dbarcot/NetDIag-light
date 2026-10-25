// Package stats implements the pure measurement math: loss, RTT summary,
// RFC 3550 jitter and the simplified E-model MOS (SPEC §7).
package stats

import (
	"math"

	"netdiag/internal/probe"
)

// LossPct is failed / (ok + failed) in percent; skipped samples are not
// part of either. 0 when nothing was sent.
func LossPct(ok, failed int) float64 {
	if ok+failed == 0 {
		return 0
	}
	return 100 * float64(failed) / float64(ok+failed)
}

// Jitter is the RFC 3550 interarrival jitter estimate over consecutive
// RTTs: J += (|D| − J) / 16, starting at 0, where D is the difference
// between successive RTTs.
func Jitter(rtts []float64) float64 {
	j := 0.0
	for i := 1; i < len(rtts); i++ {
		d := math.Abs(rtts[i] - rtts[i-1])
		j += (d - j) / 16
	}
	return j
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
// R is clamped to 0–100 first, where the polynomial is defined.
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
	Jitter                         float64
}

// Summarize computes a Summary over samples ordered oldest first. Last is
// the RTT of the newest ok sample; jitter runs over the ok RTTs in order.
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
	s.Jitter = Jitter(rtts)
	return s
}
