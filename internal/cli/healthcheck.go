package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const healthCheckTimeout = 5 * time.Second

var errUnhealthy = errors.New("healthcheck: unhealthy")

// HealthCheckCmd probes /healthz to avoid requiring curl in container images.
type HealthCheckCmd struct {
	URL string `help:"Health endpoint URL." env:"RELEASE_MIRROR_HEALTHCHECK_URL" default:"http://localhost:8080/healthz"`
}

// Run executes the health check command.
func (h *HealthCheckCmd) Run(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, http.NoBody)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := (&http.Client{Timeout: healthCheckTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", errUnhealthy, resp.Status)
	}
	return nil
}
