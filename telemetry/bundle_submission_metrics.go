package telemetry

// Fork patch (RHI-7827). See PATCH_LIST.md.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Outcomes recorded on MetricBundleSubmissionTotal. The set is closed, so the
// label's cardinality is bounded.
const (
	BundleSubmissionOutcomeAccepted    = "accepted"
	BundleSubmissionOutcomeRejected    = "rejected"
	BundleSubmissionOutcomeInvalidTx   = "invalid_tx"
	BundleSubmissionOutcomeHeadUnknown = "head_unknown"
	BundleSubmissionOutcomeAbandoned   = "abandoned"
)

var MetricBundleSubmissionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "erpc",
	Name:      "bundle_submission_total",
	Help:      "eth_sendRawTransaction requests answered by evm.bundleSubmission, by outcome: accepted (at least one target block accepted), rejected (every target block failed), invalid_tx (undecodable, nothing sent), head_unknown (no network head to target, nothing sent), abandoned (the caller went away first; submissions continued).",
}, []string{"project", "network", "outcome"})
