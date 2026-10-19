// Package inspection reads one container without changing Docker state.
package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const Timeout = 5 * time.Second

var (
	selectorPattern = regexp.MustCompile(`^/?[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	idPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// ValidSelector accepts a Docker name or ID, never a path or query.
func ValidSelector(selector string) bool {
	return selectorPattern.MatchString(selector)
}

// Result contains only metadata safe to display. Docker health is independent
// of lifecycle; a stopped container may retain an earlier health result.
type Result struct {
	Name        string
	ID          string
	Lifecycle   string
	Health      string
	CheckTime   time.Time
	Explanation string
}

// Inspect uses the caller's deadline and cancellation for SDK negotiation and
// the single container request. The CLI supplies a bounded context.
func Inspect(ctx context.Context, docker *client.Client, selector string) (Result, error) {
	if !ValidSelector(selector) {
		return Result{}, errors.New("invalid container name or ID")
	}
	response, err := docker.ContainerInspect(ctx, selector, client.ContainerInspectOptions{})
	if err != nil {
		return Result{}, requestError(ctx, err)
	}
	container := response.Container
	invalid := errors.New("Docker returned an invalid or incomplete container inspection")
	if !idPattern.MatchString(container.ID) || !ValidSelector(container.Name) || container.State == nil || container.Config == nil {
		return Result{}, invalid
	}
	state := container.State
	switch state.Status {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
	default:
		return Result{}, invalid
	}
	result := Result{
		Name: strings.TrimPrefix(container.Name, "/"), ID: container.ID,
		Lifecycle: string(state.Status), Health: "unknown",
		Explanation: "No Docker health check is configured; application health is unknown.",
	}
	check := container.Config.Healthcheck
	if check != nil && len(check.Test) > 0 && check.Test[0] != "NONE" {
		if state.Health == nil {
			result.Explanation = "Docker has not supplied a health status; application health is unknown."
		} else {
			switch state.Health.Status {
			case "healthy":
				result.Explanation = "Docker reports that the application's health check is passing."
			case "unhealthy":
				result.Explanation = "Docker reports that the application's health check has failed its configured threshold."
			case "starting":
				result.Explanation = "Docker has not yet established application health; health checks are starting."
			default:
				return Result{}, invalid
			}
			result.Health = string(state.Health.Status)
			for _, entry := range state.Health.Log {
				if entry == nil || entry.End.IsZero() {
					continue
				}
				if !entry.Start.IsZero() && entry.End.Before(entry.Start) {
					return Result{}, invalid
				}
				if entry.End.After(result.CheckTime) {
					result.CheckTime = entry.End
				}
			}
		}
	}
	if result.CheckTime.IsZero() {
		result.Explanation += " No completed health-check timestamp is available."
	}
	if result.Lifecycle != "running" {
		result.Explanation += " The container is not running normally; retained health data does not establish current application availability."
	}
	return result, nil
}

// Do not include SDK error text: daemon messages and response bodies can contain
// secrets. Error categories provide useful diagnostics without exposing them.
func requestError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return errors.New("Docker inspection canceled")
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return errors.New("Docker inspection timed out")
	case errors.Is(err, errdefs.ErrNotFound):
		return errors.New("requested container was not found")
	case errors.Is(err, errdefs.ErrPermissionDenied), errors.Is(err, errdefs.ErrUnauthenticated), errors.Is(err, os.ErrPermission),
		// shortcut: the pinned SDK discards socket errno; remove message matching when it preserves typed permission errors.
		client.IsErrConnectionFailed(err) && strings.HasPrefix(err.Error(), "permission denied while trying to connect to the docker API"):
		return errors.New("Docker access denied; check socket permissions or Docker authentication")
	case client.IsErrConnectionFailed(err):
		return errors.New("Docker is unavailable; check that the daemon is running and the connection is configured")
	default:
		return errors.New("Docker inspection failed: invalid response or Docker API request failure; check daemon availability and API compatibility")
	}
}

// Text renders only the allowlisted result fields, using one explicit time for age.
func (r Result) Text(now time.Time) string {
	check := "unavailable"
	if !r.CheckTime.IsZero() {
		age := "unknown (check timestamp is in the future; check host clocks)"
		if !r.CheckTime.After(now) {
			age = now.Sub(r.CheckTime).Round(time.Second).String()
		}
		check = fmt.Sprintf("%s (age: %s)", r.CheckTime.UTC().Format(time.RFC3339Nano), age)
	}
	return fmt.Sprintf("Container: %s\nDocker ID: %s\nLifecycle: %s\nHealth: %s\nLast completed health check: %s\nExplanation: %s\nAction: none — inspection only.\n",
		r.Name, r.ID, r.Lifecycle, r.Health, check, r.Explanation)
}
