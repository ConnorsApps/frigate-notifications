// Package httpserver provides a graceful-shutdown run loop shared by every
// HTTP server in this environment.
package httpserver

import (
	"context"
	"net/http"
)

// RunGraceful starts an http.Server on addr with handler, blocking until
// ctx is cancelled (at which point it shuts the server down) or
// ListenAndServe returns a non-http.ErrServerClosed error.
func RunGraceful(ctx context.Context, addr string, handler http.Handler) error {
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
