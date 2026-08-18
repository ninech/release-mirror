package server

import (
	"net/http"

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

// Transport returns an [http.RoundTripper] configured with user agent, trace ID, and metrics.
func (s *Server) Transport() http.RoundTripper {
	return transport.Chain(
		gzhttp.Transport(http.DefaultTransport),
		transport.SetHeader("User-Agent", s.buildInfo.Name+"/"+s.buildInfo.Version),
		traceid.Transport,
		metrics.Transport(metrics.TransportOpts{Host: true}),
	)
}
