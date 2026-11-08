package zfspulse

import "time"

const (
	// DefaultProcZfsPath is the standard Linux OpenZFS procfs kstat directory path.
	DefaultProcZfsPath = "/proc/spl/kstat/zfs"

	// DefaultScrubDriftThresholdHours is 30 days (720 hours) before scrub drift penalty begins.
	DefaultScrubDriftThresholdHours = 720.0

	// DefaultCommandTimeout is the execution timeout for zpool CLI subprocesses.
	DefaultCommandTimeout = 15 * time.Second
)

// Options holds runtime configuration for the Inspector.
type Options struct {
	// ProcZfsPath specifies the Linux OpenZFS kstat procfs path.
	// Defaults to DefaultProcZfsPath. Set to empty string to disable procfs fast-path.
	ProcZfsPath string

	// ScrubDriftThresholdHours defines the threshold in hours after which scrub delay penalizes reliability.
	// Defaults to 720.0 (30 days).
	ScrubDriftThresholdHours float64

	// CommandTimeout limits subprocess execution to protect against kernel D-state deadlocks.
	// Defaults to 15s.
	CommandTimeout time.Duration
}

// Option configures an Options instance.
type Option func(*Options)

// DefaultOptions returns the baseline options complying with NIS2 and DORA standards.
func DefaultOptions() Options {
	return Options{
		ProcZfsPath:              DefaultProcZfsPath,
		ScrubDriftThresholdHours: DefaultScrubDriftThresholdHours,
		CommandTimeout:           DefaultCommandTimeout,
	}
}

// WithProcZfsPath sets the procfs kstat base path.
func WithProcZfsPath(path string) Option {
	return func(o *Options) {
		o.ProcZfsPath = path
	}
}

// WithScrubDriftThresholdHours sets the scrub drift threshold in hours.
func WithScrubDriftThresholdHours(hours float64) Option {
	return func(o *Options) {
		if hours > 0 {
			o.ScrubDriftThresholdHours = hours
		}
	}
}

// WithCommandTimeout sets the execution timeout for zpool commands.
func WithCommandTimeout(d time.Duration) Option {
	return func(o *Options) {
		if d > 0 {
			o.CommandTimeout = d
		}
	}
}
