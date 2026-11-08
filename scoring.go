package zfspulse

// stateSeverity defines the severity rank of pool states (higher = worse/more critical).
var stateSeverity = map[string]int{
	"UNKNOWN":   0,
	"ONLINE":    1,
	"DEGRADED":  2,
	"OFFLINE":   3,
	"UNAVAIL":   4,
	"FAULTED":   5,
	"REMOVED":   5,
	"SUSPENDED": 5,
}

// RaiseState elevates the pool state if the new state represents a higher severity.
// Lower severity states never overwrite already degraded or faulted states.
// When newState is UNKNOWN, it resets an unverified ONLINE state to UNKNOWN.
func (h *PoolHealth) RaiseState(newState string) {
	if h.State == "" {
		h.State = "UNKNOWN"
	}
	if newState == "UNKNOWN" {
		if h.State == "ONLINE" {
			h.State = "UNKNOWN"
		}
		return
	}
	if stateSeverity[newState] > stateSeverity[h.State] {
		h.State = newState
	}
}

// CalculateReliabilityScore calculates the 0-100% health reliability index as a pure, deterministic function.
func CalculateReliabilityScore(h *PoolHealth, thresholdHours float64) float64 {
	return calculateReliabilityScore(h, thresholdHours)
}

// calculateReliabilityScore calculates the 0-100% health reliability index as a pure, deterministic function.
func calculateReliabilityScore(h *PoolHealth, thresholdHours float64) float64 {
	score := 100.0
	switch h.State {
	case "ONLINE":
		score = 100.0
	case "DEGRADED":
		score = 40.0
	case "FAULTED", "UNAVAIL", "OFFLINE", "REMOVED", "SUSPENDED", "UNKNOWN":
		score = 0.0
	default:
		score = 0.0
	}

	if !h.ScrubInProgress && h.ScrubDriftHours > thresholdHours {
		excess := h.ScrubDriftHours - thresholdHours
		penalty := (excess / 24.0) * 5.0
		score -= penalty
	}

	score -= float64(h.ReadErrors) * 10.0
	score -= float64(h.WriteErrors) * 15.0
	score -= float64(h.ChecksumErrors) * 25.0

	if score < 0.0 {
		score = 0.0
	}
	return score
}
