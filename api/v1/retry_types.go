package v1

// RetryStatus is how a resource that writes to Azure API Management backs off after
// failed writes. It is embedded inline in the status of every kind that writes to APIM
// (APIMAPIDeployment, APIMProduct, APIMTag, APIMInboundPolicy), so the fields read the
// same on all of them.
//
// After a failed write the operator waits until nextAttemptAt before writing again, with
// the wait doubling per failure. After five transient failures in a row the phase becomes
// Stalled; a request APIM rejects outright (400, 401, 403, or 404 on the resource's own
// path) makes it Invalid at once. Neither is retried until the spec changes or the
// apim.operator.io/retry annotation is set to a new value. A 404 because something the
// write hangs off is not in APIM yet (the product or tag an API is assigned to, the API a
// policy is set on) counts as transient: it usually clears once that resource is written.
type RetryStatus struct {
	// ConsecutiveFailures counts the failed APIM writes in a row since the last success,
	// spec change or retry annotation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// NextAttemptAt is the earliest time (RFC3339) the operator writes to APIM again.
	// Empty when no retry is scheduled: the last write succeeded, or the resource is
	// Stalled or Invalid.
	// +optional
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	// LastRetryAnnotation is the value of the apim.operator.io/retry annotation the
	// operator last acted on. Setting the annotation to any other value clears the
	// failure count and retries at once.
	// +optional
	LastRetryAnnotation string `json:"lastRetryAnnotation,omitempty"`
}
