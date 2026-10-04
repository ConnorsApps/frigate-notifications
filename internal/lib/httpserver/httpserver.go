// Package httpserver provides a graceful-shutdown run loop shared by every
// HTTP server in this environment.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Limits on a connection that isn't doing anything. No read or write timeout:
// the media proxy streams clips for as long as a phone takes to download them.
// Vars so tests can shorten them.
var (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	// shutdownTimeout is how long in-flight requests get to finish once the
	// context ends, before they are cut off.
	shutdownTimeout = 10 * time.Second
)

// RunGraceful starts an http.Server on addr with handler. It blocks until the
// server fails (returning the error) or ctx is canceled, then stops accepting
// connections and waits for in-flight requests, up to shutdownTimeout, before
// returning nil.
func RunGraceful(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// ListenAndServe returns the moment Shutdown starts, so waiting for it
	// proves nothing: Shutdown itself has to finish before the caller exits.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Past the deadline: drop what is left rather than hang the exit.
	srv.Close()
	return nil
}
