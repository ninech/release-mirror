package server

import (
	"net/http"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-chi/chi/v5"
)

// sentryHandler returns the Sentry request middleware.
func sentryHandler() func(http.Handler) http.Handler {
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle
}

// sentryRouteName names the transaction after the matched chi route pattern.
func sentryRouteName(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pattern is only known once routing has run, so rename on the way out.
		// This defer runs before sentryhttp finishes the transaction.
		defer nameTransaction(r)

		next.ServeHTTP(w, r)
	})
}

// nameTransaction replaces the transaction name of r with its chi route pattern.
// It is a no-op when r is not instrumented or did not match a route.
func nameTransaction(r *http.Request) {
	ctx := r.Context()
	tx := sentry.TransactionFromContext(ctx)
	rctx := chi.RouteContext(ctx)
	if tx == nil || rctx == nil {
		return
	}
	pattern := rctx.RoutePattern()
	if pattern == "" {
		return
	}
	tx.Name = r.Method + " " + pattern
	tx.Source = sentry.SourceRoute
}
