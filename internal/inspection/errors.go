package inspection

import "errors"

// Collection failures carry only Aegis-authored messages, never daemon payloads.
type collectionError struct{ category, message string }

func (e *collectionError) Error() string { return e.message }

// FailureDetails supplies a bounded, sanitized explanation even for unknown errors.
func FailureDetails(err error) (category, explanation string) {
	var failure *collectionError
	if errors.As(err, &failure) {
		return failure.category, failure.message
	}
	return "request_failed", "Docker inspection failed; no application health was observed."
}
