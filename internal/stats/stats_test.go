package stats

import (
	"math"
	"testing"
	"time"

	"netdiag/internal/probe"
)

const eps = 1e-6

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestLossPct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ok, failed int
		want       float64
	}{
		{0, 0, 0},
		{10, 0, 0},
		{0, 4, 100},
		{9, 1, 10},
		{3, 1, 25},
		{2, 1, 100.0 / 3},
	}
	for _, tt := range tests {
		if got := LossPct(tt.ok, tt.failed); !near(got, tt.want, eps) {
			t.Errorf("LossPct(%d, %d) = %v, want %v", tt.ok, tt.failed, got, tt.want)
		}
	}
}

// seq builds samples from RTTs in ms; a negative value is a failed sample, NaN a skipped one.
func seq(vals ...float64) []probe.Sample {
	out := make([]probe.Sample, len(vals))
	for i, v := range vals {
		switch {
		case math.IsNaN(v):
			out[i] = probe.Sample{Result: probe.Skipped}
		case v < 0:
			out[i] = probe.Sample{Result: probe.Fail}
		default:
			out[i] = probe.Sample{Result: probe.OK, RTT: ms(v)}
		}
	}
	return out
}

func TestJitter_MeanAbsoluteDifference(t *testing.T) {
	t.Parallel()
	skip := math.NaN()
	tests := []struct {
		name    string
		samples []probe.Sample
		want    float64
		ok      bool
	}{
		// constant |D| = 5: exact from the second pair on (no start-up bias)
		{"constant |D|, 2 pairs", seq(10, 15, 10), 5, true},
		{"constant |D|, many pairs", seq(10, 15, 20, 25, 20, 15), 5, true},
		{"alternating 10/30 ms", seq(10, 30, 10, 30, 10, 30, 10), 20, true},
		{"flat", seq(20, 20, 20, 20), 0, true},
		// pairs: (10,14)=4, then a failure breaks the chain, (30,30)=0, (30,32)=2 → 6/3 = 2
		{"gap across a failure is ignored", seq(10, 14, -1, 30, 30, 32), 2, true},
		// (10,14)=4, skipped breaks, (50,52)=2 → 3
		{"gap across a skipped sample is ignored", seq(10, 14, skip, 50, 52), 3, true},
		{"single ok sample: unknown", seq(10), 0, false},
		{"one pair: unknown", seq(10, 14), 0, false},
		{"no usable pair: unknown", seq(10, -1, 14, skip, 30), 0, false},
		{"empty: unknown", nil, 0, false},
	}
	for _, tt := range tests {
		got, ok := Jitter(tt.samples)
		if ok != tt.ok || !near(got, tt.want, eps) {
			t.Errorf("%s: Jitter = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestStdDev_Population(t *testing.T) {
	t.Parallel()
	// 2,4,4,4,5,5,7,9: mean 5, squared deviations sum 32, /8 = 4 → 2
	tests := []struct {
		v    []float64
		want float64
	}{
		{nil, 0},
		{[]float64{7}, 0},
		{[]float64{2, 4, 4, 4, 5, 5, 7, 9}, 2},
		{[]float64{1, 3}, 1},
	}
	for _, tt := range tests {
		if got := StdDev(tt.v); !near(got, tt.want, eps) {
			t.Errorf("StdDev(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestMOS_KnownPoints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		rtt, jitter, loss float64
		want              float64
	}{
		// eff = 30, R = 93.2 − 0.75 = 92.45
		// MOS = 1 + 3.23575 + 7e-6·92.45·32.45·7.55 = 1 + 3.23575 + 0.158557… = 4.394307…
		{"20 ms, no jitter, no loss", 20, 0, 0, 4.394307},
		// eff = 160 → second branch: R = 93.2 − 4 = 89.2
		// MOS = 1 + 3.122 + 7e-6·89.2·29.2·10.8 = 1 + 3.122 + 0.196910… = 4.318910…
		{"eff exactly 160", 150, 0, 0, 4.318910},
		// eff = 10 + 2·10 + 10 = 40, R = 93.2 − 1 − 2.5·10 = 67.2
		// MOS = 1 + 2.352 + 7e-6·67.2·7.2·32.8 = 1 + 2.352 + 0.111091… = 3.463091…
		{"10 ms, 10 ms jitter, 10 % loss", 10, 10, 10, 3.463091},
		// eff = 310, R = 93.2 − 19 = 74.2
		// MOS = 1 + 2.597 + 7e-6·74.2·14.2·25.8 = 1 + 2.597 + 0.190293… = 3.787293…
		{"300 ms RTT", 300, 0, 0, 3.787293},
		// R = 93.2 − 0.75 − 250 < 0 → clamped to 0 → MOS 1
		{"100 % loss clamps to 1", 20, 0, 100, 1},
		// eff = 10, R = 93.2 − 0.25 = 92.95
		// MOS = 1 + 3.25325 + 7e-6·92.95·32.95·7.05 = 1 + 3.25325 + 0.151144… = 4.404394… (< 4.5)
		{"zero RTT stays below 4.5", 0, 0, 0, 4.404394},
	}
	for _, tt := range tests {
		if got := MOS(tt.rtt, tt.jitter, tt.loss); !near(got, tt.want, 1e-5) {
			t.Errorf("%s: MOS = %.6f, want %.6f", tt.name, got, tt.want)
		}
	}
}

func TestMOS_AlwaysWithinBounds(t *testing.T) {
	t.Parallel()
	for _, rtt := range []float64{0, 1, 50, 159, 160, 500, 5000} {
		for _, j := range []float64{0, 5, 100, 1000} {
			for _, l := range []float64{0, 1, 20, 50, 100} {
				if m := MOS(rtt, j, l); m < 1 || m > 4.5 || math.IsNaN(m) {
					t.Fatalf("MOS(%v, %v, %v) = %v out of 1–4.5", rtt, j, l, m)
				}
			}
		}
	}
}

func ms(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }

func TestSummarize(t *testing.T) {
	t.Parallel()
	samples := []probe.Sample{
		{Result: probe.OK, RTT: ms(10)},
		{Result: probe.Fail, ErrClass: probe.ClassTimeout},
		{Result: probe.Skipped, Detail: "skipped: gateway down"},
		{Result: probe.OK, RTT: ms(14)},
		{Result: probe.Skipped},
		{Result: probe.OK, RTT: ms(10)},
		{Result: probe.OK, RTT: ms(10)},
		{Result: probe.Fail},
	}
	s := Summarize(samples)
	// sent = 4 ok + 2 failed; skipped never counts as loss: 2/6 = 33.33 %
	if s.Sent != 6 || s.OK != 4 || s.Failed != 2 || s.Skipped != 2 || !near(s.LossPct, 100.0/3, eps) {
		t.Errorf("counts %+v", s)
	}
	// RTTs 10, 14, 10, 10: avg 11, best 10, worst 14, last 10,
	// stdev sqrt(((−1)²+3²+(−1)²+(−1)²)/4) = sqrt(3)
	if !s.HasRTT || !near(s.Avg, 11, eps) || s.Best != 10 || s.Worst != 14 || s.Last != 10 ||
		!near(s.StdDev, math.Sqrt(3), eps) {
		t.Errorf("rtt stats %+v", s)
	}
	// only one pair of consecutive ok samples (10, 10): jitter unknown
	if s.HasJitter {
		t.Errorf("jitter must be unknown with one usable pair: %+v", s)
	}
}

func TestSummarize_Jitter(t *testing.T) {
	t.Parallel()
	s := Summarize(seq(10, 30, 10, -1, 30, 10))
	// pairs (10,30)=20, (30,10)=20, failure, (30,10)=20 → 20
	if !s.HasJitter || !near(s.Jitter, 20, eps) {
		t.Errorf("jitter %v (known %v), want 20", s.Jitter, s.HasJitter)
	}
}

func TestSummarize_OnlySkippedIsNotLoss(t *testing.T) {
	t.Parallel()
	s := Summarize([]probe.Sample{{Result: probe.Skipped}, {Result: probe.Skipped}})
	if s.Sent != 0 || s.LossPct != 0 || s.HasRTT || s.Skipped != 2 {
		t.Errorf("got %+v", s)
	}
}

func TestSummarize_AllFailed(t *testing.T) {
	t.Parallel()
	s := Summarize([]probe.Sample{{Result: probe.Fail}, {Result: probe.Fail}})
	if s.LossPct != 100 || s.HasRTT {
		t.Errorf("got %+v", s)
	}
}

func TestSummarize_Empty(t *testing.T) {
	t.Parallel()
	if s := Summarize(nil); s != (Summary{}) {
		t.Errorf("got %+v", s)
	}
}
