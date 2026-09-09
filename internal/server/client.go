package server

import (
	"net/http"

	sentryhttpclient "github.com/getsentry/sentry-go/httpclient"
	"github.com/go-chi/metrics"
	"github.com/go-chi/traceid"
	"github.com/go-chi/transport"
	"github.com/klauspost/compress/gzhttp"
)

// HTTPClient returns an [http.Client] configured with trace ID, metrics, and compression.
func (s *Server) HTTPClient() *http.Client {
	return &http.Client{
		Transport: s.Transport(),
		Timeout:   DefaultTimeout,
	}
}

// noTracePropagation is a target that cannot occur in a request URL. It disables
// trace propagation, which sentryhttpclient has no explicit option for: it reads
// an empty target list as "every host is a target".
const noTracePropagation = "\x00"

// Transport returns an [http.RoundTripper] configured with user agent, trace ID, and metrics.
// With [WithSentry] it also records an http.client span per outgoing request to a
// trace propagation target and propagates the trace to it.
func (s *Server) Transport() http.RoundTripper {
	// Chain drops nil middlewares, so a disabled Sentry adds nothing to the chain.
	var sentryTransport func(http.RoundTripper) http.RoundTripper
	if s.sentry {
		targets := s.sentryTargets
		if len(targets) == 0 {
			targets = []string{noTracePropagation}
		}
		sentryTransport = func(next http.RoundTripper) http.RoundTripper {
			return sentryhttpclient.NewSentryRoundTripper(next,
				sentryhttpclient.WithTracePropagationTargets(targets))
		}
	}

	return transport.Chain(
		gzhttp.Transport(http.DefaultTransport),
		transport.SetHeader("User-Agent", s.buildInfo.Name+"/"+s.buildInfo.Version),
		traceid.Transport,
		metrics.Transport(metrics.TransportOpts{Host: true}),
		sentryTransport,
	)
}
