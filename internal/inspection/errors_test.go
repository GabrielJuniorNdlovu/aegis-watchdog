package inspection

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
)

func TestFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		err      error
		category string
	}{
		{context.DeadlineExceeded, "timeout"}, {context.Canceled, "canceled"},
		{errdefs.ErrNotFound, "not_found"}, {errdefs.ErrPermissionDenied, "permission_denied"},
		{&json.SyntaxError{}, "invalid_response"}, {errors.New("SECRET"), "request_failed"},
	} {
		category, message := FailureDetails(requestError(context.Background(), tc.err))
		if category != tc.category || len(message) > 256 || strings.Contains(message, "SECRET") {
			t.Fatalf("category=%s message=%q", category, message)
		}
	}
}
