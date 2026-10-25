package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/JorisJonkers-dev/delivery/internal/gate"
)

// maxReleases bounds how much of the gate's answer is read.
const maxReleases = 4 << 20

// GateReleases reads the Release Gate's GET /releases, over the Reporter's edge to its `http`
// surface.
type GateReleases struct {
	// URL is the gate's address, without a path.
	URL  string
	HTTP *http.Client
}

var _ Gate = GateReleases{}

// Releases implements Gate.
func (g GateReleases) Releases(ctx context.Context) (gate.Releases, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+gate.PathReleases, nil)
	if err != nil {
		return gate.Releases{}, err
	}
	res, err := g.HTTP.Do(req)
	if err != nil {
		return gate.Releases{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return gate.Releases{}, fmt.Errorf("reporter: the Release Gate answered %d", res.StatusCode)
	}
	var releases gate.Releases
	if err := json.NewDecoder(io.LimitReader(res.Body, maxReleases)).Decode(&releases); err != nil {
		return gate.Releases{}, fmt.Errorf("reporter: the Release Gate's releases do not parse: %w", err)
	}
	return releases, nil
}
