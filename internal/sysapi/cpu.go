package sysapi

// CPUTimes are cumulative system times in 100 ns units, as returned by
// GetSystemTimes. Kernel includes Idle.
type CPUTimes struct {
	Idle, Kernel, User uint64
}

// CPUUsage returns total CPU usage in percent between two readings. ok is
// false when no time passed between them. Counter wraparound is handled
// by unsigned subtraction.
func CPUUsage(prev, cur CPUTimes) (pct float64, ok bool) {
	idle := cur.Idle - prev.Idle
	total := (cur.Kernel - prev.Kernel) + (cur.User - prev.User)
	if total == 0 {
		return 0, false
	}
	if idle > total {
		idle = total
	}
	return 100 * float64(total-idle) / float64(total), true
}

// CPUMeter turns successive CPU readings into usage percentages.
type CPUMeter struct {
	src  CPU
	prev CPUTimes
	have bool
}

// NewCPUMeter returns a meter reading from src.
func NewCPUMeter(src CPU) *CPUMeter { return &CPUMeter{src: src} }

// Sample reads the CPU times and returns usage since the previous call.
// ok is false on the first call and when no time has passed.
func (m *CPUMeter) Sample() (pct float64, ok bool, err error) {
	cur, err := m.src.Times()
	if err != nil {
		return 0, false, err
	}
	prev, had := m.prev, m.have
	if had {
		pct, ok = CPUUsage(prev, cur)
		if !ok {
			return 0, false, nil // keep prev so the next delta spans the gap
		}
	}
	m.prev, m.have = cur, true
	return pct, ok, nil
}

// CPULevel is the console/web CPU load label.
type CPULevel string

const (
	CPULight    CPULevel = "Light"
	CPUModerate CPULevel = "Moderate"
	CPUHigh     CPULevel = "High"
	CPUHeavy    CPULevel = "Heavy"
)

// LevelForCPU maps a usage percentage to a level. Each boundary belongs to
// the higher level (pct >= high → High), matching CPU_HIGH (SPEC §8).
func LevelForCPU(pct float64, moderate, high, heavy int) CPULevel {
	switch {
	case pct >= float64(heavy):
		return CPUHeavy
	case pct >= float64(high):
		return CPUHigh
	case pct >= float64(moderate):
		return CPUModerate
	default:
		return CPULight
	}
}
