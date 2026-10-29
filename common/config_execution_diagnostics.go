package common

import (
	"fmt"
	"math"

	"github.com/erpc/erpc/util"
)

const (
	DefaultExecutionDiagnosticsMaxEventsPerSecond         = 10
	DefaultExecutionDiagnosticsBurst                      = 100
	DefaultExecutionDiagnosticsMaxAttemptEventsPerRequest = 8
)

// ExecutionDiagnosticsConfig controls the payload-free execution diagnostics
// log stream (per-request upstream invocation evidence and breaker
// transitions). The stream is independent of logLevel; its volume is bounded
// per replica by a token bucket counted in log lines.
type ExecutionDiagnosticsConfig struct {
	Enabled                    *bool   `yaml:"enabled,omitempty" json:"enabled"`
	MaxEventsPerSecond         float64 `yaml:"maxEventsPerSecond,omitempty" json:"maxEventsPerSecond"`
	Burst                      int     `yaml:"burst,omitempty" json:"burst"`
	MaxAttemptEventsPerRequest int     `yaml:"maxAttemptEventsPerRequest,omitempty" json:"maxAttemptEventsPerRequest"`
}

func (c *ExecutionDiagnosticsConfig) SetDefaults() {
	if c.Enabled == nil {
		c.Enabled = util.BoolPtr(true)
	}
	if c.MaxEventsPerSecond == 0 {
		c.MaxEventsPerSecond = DefaultExecutionDiagnosticsMaxEventsPerSecond
	}
	if c.MaxAttemptEventsPerRequest == 0 {
		c.MaxAttemptEventsPerRequest = DefaultExecutionDiagnosticsMaxAttemptEventsPerRequest
	}
	if c.Burst == 0 {
		c.Burst = DefaultExecutionDiagnosticsBurst
		// A full request (summary + capped attempts) must always fit the bucket.
		if min := 1 + c.MaxAttemptEventsPerRequest; c.Burst < min {
			c.Burst = min
		}
	}
}

func (c *ExecutionDiagnosticsConfig) Validate() error {
	if c.MaxEventsPerSecond <= 0 || math.IsNaN(c.MaxEventsPerSecond) || math.IsInf(c.MaxEventsPerSecond, 0) {
		return fmt.Errorf("executionDiagnostics.maxEventsPerSecond must be > 0, got %v", c.MaxEventsPerSecond)
	}
	if c.MaxAttemptEventsPerRequest < 1 {
		return fmt.Errorf("executionDiagnostics.maxAttemptEventsPerRequest must be >= 1, got %d", c.MaxAttemptEventsPerRequest)
	}
	if c.Burst < 1+c.MaxAttemptEventsPerRequest {
		return fmt.Errorf("executionDiagnostics.burst must be >= 1 + maxAttemptEventsPerRequest (%d), got %d", 1+c.MaxAttemptEventsPerRequest, c.Burst)
	}
	return nil
}

// IsEnabled reports whether the stream is on; a nil config means defaults (on).
func (c *ExecutionDiagnosticsConfig) IsEnabled() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}
