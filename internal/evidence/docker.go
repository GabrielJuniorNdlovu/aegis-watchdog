package evidence

import (
	"errors"
	"io"
	"net/http"

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
