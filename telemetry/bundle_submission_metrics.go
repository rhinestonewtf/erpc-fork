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

// Outcomes recorded on MetricBundleKeepAliveTotal: how a kept transaction
// stopped being resubmitted.
const (
	BundleKeepAliveOutcomeSettled  = "settled"
	BundleKeepAliveOutcomeExpired  = "expired"
	BundleKeepAliveOutcomeOverflow = "overflow"
	BundleKeepAliveOutcomeDisabled = "disabled"
)

var MetricBundleSubmissionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "erpc",
	Name:      "bundle_submission_total",
	Help:      "eth_sendRawTransaction requests answered by evm.bundleSubmission, by outcome: accepted (at least one target block accepted), rejected (every target block failed), invalid_tx (undecodable or for another chain, nothing sent), head_unknown (no network head to target, nothing sent), abandoned (the caller went away first; submissions continued).",
}, []string{"project", "network", "outcome"})

var MetricBundleKeepAliveTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "erpc",
	Name:      "bundle_keepalive_total",
	Help:      "Accepted transactions that stopped being resubmitted, by outcome: settled (the sender's nonce passed the transaction: included or replaced), expired (resubmitFor ran out with the nonce unmoved: the transaction was dropped), overflow (not kept because too many were already kept), disabled (bundleSubmission was removed from the network).",
}, []string{"project", "network", "outcome"})

var MetricBundleKeepAliveTracked = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "erpc",
	Name:      "bundle_keepalive_tracked",
	Help:      "Accepted transactions currently being resubmitted for each new block.",
}, []string{"project", "network"})
