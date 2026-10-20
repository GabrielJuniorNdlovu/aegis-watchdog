package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/store"
)

func errorHistory(ctx context.Context, path, target string, limit int, stdout, stderr io.Writer) int {
	failures, err := store.ErrorHistory(ctx, path, target, limit)
	if err != nil {
		fmt.Fprintln(stderr, "aegis: cannot read collection errors:", err)
		return 1
	}
	var output strings.Builder
	if len(failures) == 0 {
		fmt.Fprintf(&output, "No saved collection errors for requested target %s.\n", target)
	} else {
		fmt.Fprintf(&output, "Collection errors for requested target: %s\nHistorical collection failures; these are not application health results.\n", target)
		for _, failure := range failures {
			fmt.Fprintf(&output, "\nCollected at: %s\nCategory: %s\nExplanation: %s\n", failure.CollectedAt.UTC().Format(time.RFC3339Nano), failure.Category, failure.Explanation)
		}
	}
	if _, err := io.WriteString(stdout, output.String()); err != nil {
		fmt.Fprintln(stderr, "aegis: cannot write collection errors output")
		return 1
	}
	return 0
}
