package zfspulse

// Alert threshold and hysteresis constants complying with NIS2 and DORA ICT risk monitoring standards.
const (
	ScoreWarnEnter     = 70.0
	ScoreWarnExit      = 75.0 // +5% hysteresis
	ScoreCriticalEnter = 50.0
	ScoreCriticalExit  = 55.0 // +5% hysteresis
	ScoreDeltaTrigger  = 10.0 // Significant score change threshold
)

// Severity represents the high-level health severity classification of a pool.
type Severity string

const (
	SeverityOK       Severity = "OK"
	SeverityWarn     Severity = "WARN"
	SeverityCritical Severity = "CRITICAL"
	SeverityUnknown  Severity = "UNKNOWN"
)

// PoolHealth represents parsed metrics and operational status for a single ZFS storage pool.
type PoolHealth struct {
	Name             string  `json:"name"`
	State            string  `json:"state"` // ONLINE, DEGRADED, FAULTED, UNAVAIL, OFFLINE, REMOVED, SUSPENDED, UNKNOWN
	ReadErrors       uint64  `json:"read_errors"`
	WriteErrors      uint64  `json:"write_errors"`
	ChecksumErrors   uint64  `json:"checksum_errors"`
	ScrubDriftHours  float64 `json:"scrub_drift_hours"`
	ReliabilityScore float64 `json:"reliability_score"` // 0.0 - 100.0%
	StatusNote       string  `json:"status_note,omitempty"`
	ScrubInProgress  bool    `json:"scrub_in_progress,omitempty"`
}

// Severity returns the evaluated operational severity of the pool.
func (h PoolHealth) Severity() Severity {
	switch h.State {
	case "ONLINE":
		if h.ReliabilityScore < ScoreCriticalEnter {
			return SeverityCritical
		}
		if h.ReliabilityScore < ScoreWarnExit {
			return SeverityWarn
		}
		return SeverityOK
	case "UNKNOWN":
		return SeverityUnknown
	case "DEGRADED", "FAULTED", "UNAVAIL", "OFFLINE", "REMOVED", "SUSPENDED":
		return SeverityCritical
	default:
		return SeverityUnknown
	}
}
