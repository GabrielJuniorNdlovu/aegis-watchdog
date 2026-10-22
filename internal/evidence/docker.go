package evidence

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/GabrielJuniorNdlovu/aegis-watchdog/internal/inspection"
	"github.com/moby/moby/client"
)

// Inspection bodies and Docker error bodies need a bound too: the SDK decodes
// complete inspect responses before our allowlist runs. Logs have a smaller,
// separately configured wire allowance in readLogs.
const MaxDockerResponse = 1024 * 1024

type DockerClient struct{ sdk *client.Client }

func NewDockerClient(options ...client.Opt) (*DockerClient, error) {
	options = append(options, client.WithHTTPResponseHook(func(response *http.Response) {
		response.Body = &responseBody{ReadCloser: response.Body, remaining: MaxDockerResponse}
	}))
	sdk, err := client.New(options...)
	if err != nil {
		return nil, err
	}
	return &DockerClient{sdk}, nil
}
func (d *DockerClient) Close() error { return d.sdk.Close() }

func (d *DockerClient) Inspect(ctx context.Context, id string) (inspection.Result, error) {
	return inspection.Inspect(ctx, d.sdk, id)
}

// Restart sends one Docker restart request. The caller persists dispatching
// before invoking it; Moby's SDK issues one POST and does not retry it.
func (d *DockerClient) Restart(ctx context.Context, id string, graceful time.Duration) error {
	if !inspection.ValidID(id) || graceful < 0 || graceful > 2*time.Minute {
		return errors.New("invalid bounded restart request")
	}
	seconds := int(graceful.Round(time.Second) / time.Second)
	_, err := d.sdk.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &seconds})
	return err
}

type responseBody struct {
	io.ReadCloser
	remaining int64
}

func (b *responseBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, errors.New("Docker evidence response allowance exceeded")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}
