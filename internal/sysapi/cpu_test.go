package sysapi

import (
	"errors"
	"math"
	"testing"
)

func TestCPUUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		prev, cur CPUTimes
		want      float64
		ok        bool
	}{
		{"idle machine", CPUTimes{0, 0, 0}, CPUTimes{Idle: 1000, Kernel: 1000, User: 0}, 0, true},
		{"fully busy", CPUTimes{}, CPUTimes{Idle: 0, Kernel: 500, User: 500}, 100, true},
		{"quarter busy", CPUTimes{}, CPUTimes{Idle: 750, Kernel: 900, User: 100}, 25, true},
		{"from non-zero base", CPUTimes{100, 200, 300}, CPUTimes{Idle: 600, Kernel: 900, User: 600}, 50, true},
		{"zero delta", CPUTimes{5, 10, 15}, CPUTimes{5, 10, 15}, 0, false},
		{"idle above total clamps to 0 %", CPUTimes{}, CPUTimes{Idle: 2000, Kernel: 1000, User: 0}, 0, true},
	}
	for _, tt := range tests {
		got, ok := CPUUsage(tt.prev, tt.cur)
		if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: CPUUsage = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCPUUsage_WraparoundExact(t *testing.T) {
	t.Parallel()
	// Each counter crosses 2^64: idle +200, kernel +400, user +100 → 300 of 500 busy = 60 %.
	prev := CPUTimes{Idle: math.MaxUint64 - 99, Kernel: math.MaxUint64 - 199, User: math.MaxUint64 - 49}
	cur := CPUTimes{Idle: 100, Kernel: 200, User: 50}
	if got, ok := CPUUsage(prev, cur); !ok || math.Abs(got-60) > 1e-9 {
		t.Errorf("CPUUsage = %v, %v; want 60, true", got, ok)
	}
}

type seqCPU struct {
	times []CPUTimes
	err   error
}

func (s *seqCPU) Times() (CPUTimes, error) {
	if s.err != nil {
		return CPUTimes{}, s.err
	}
	t := s.times[0]
	s.times = s.times[1:]
	return t, nil
}

func TestCPUMeter(t *testing.T) {
	t.Parallel()
	src := &seqCPU{times: []CPUTimes{
		{Idle: 0, Kernel: 0, User: 0},
		{Idle: 50, Kernel: 100, User: 100},  // 150/200 busy = 75 %
		{Idle: 50, Kernel: 100, User: 100},  // no time passed
		{Idle: 150, Kernel: 200, User: 100}, // vs reading 2: idle 100 of 100 → 0 %
	}}
	m := NewCPUMeter(src)
	if _, ok, err := m.Sample(); ok || err != nil {
		t.Fatalf("first sample must only prime: ok=%v err=%v", ok, err)
	}
	if pct, ok, _ := m.Sample(); !ok || pct != 75 {
		t.Errorf("second sample = %v, %v; want 75", pct, ok)
	}
	if _, ok, _ := m.Sample(); ok {
		t.Error("zero delta must not produce a sample")
	}
	if pct, ok, _ := m.Sample(); !ok || pct != 0 {
		t.Errorf("fourth sample = %v, %v; want 0", pct, ok)
	}
	src.err = errors.New("boom")
	if _, _, err := m.Sample(); err == nil {
		t.Error("source error not returned")
	}
}

func TestLevelForCPU_Boundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pct  float64
		want CPULevel
	}{
		{0, CPULight},
		{49.99, CPULight},
		{50, CPUModerate},
		{74.99, CPUModerate},
		{75, CPUHigh},
		{89.99, CPUHigh},
		{90, CPUHeavy},
		{100, CPUHeavy},
	}
	for _, tt := range tests {
		if got := LevelForCPU(tt.pct, 50, 75, 90); got != tt.want {
			t.Errorf("LevelForCPU(%v) = %s, want %s", tt.pct, got, tt.want)
		}
	}
}
