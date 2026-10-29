package execdiag

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type inboundTraceKey struct{}

// WithInboundTraceContext records the caller's W3C trace context for
// diagnostics only. It is extracted into a detached context and stored under
// a private key, so it never becomes the active span context and is never
// injected into outbound provider requests.
func WithInboundTraceContext(ctx context.Context, h http.Header) context.Context {
	if h.Get("traceparent") == "" {
		return ctx
	}
	sc := trace.SpanContextFromContext(
		propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h)),
	)
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, inboundTraceKey{}, sc)
}

type traceIDs struct {
	traceID      string
	spanID       string
	parentSpanID string
}

// resolveTrace prefers eRPC's own active span; otherwise falls back to the
// caller's inbound trace (whose span id is our parent, not ours).
func resolveTrace(ctx context.Context) traceIDs {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return traceIDs{traceID: sc.TraceID().String(), spanID: sc.SpanID().String()}
	}
	if sc, ok := ctx.Value(inboundTraceKey{}).(trace.SpanContext); ok && sc.IsValid() {
		return traceIDs{traceID: sc.TraceID().String(), parentSpanID: sc.SpanID().String()}
	}
	return traceIDs{}
}
