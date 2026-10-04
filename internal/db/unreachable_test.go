package db

import (
	"context"
	"testing"
	"time"
)

// A database deployed beside the app may come up after it. The server must
// keep the connection and retry its setup rather than give up for the whole
// process lifetime; a one-shot tool has nothing to wait for and fails.
func TestUnreachableDatabase(t *testing.T) {
	for name, url := range map[string]string{
		"postgres": "postgres://u:p@127.0.0.1:1/db?connect_timeout=1",
		"mongo":    "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=300&connectTimeoutMS=300",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			d, err := Open(ctx, url, nil)
			if err != nil {
				t.Fatalf("Open (server mode) = %v, want a usable handle that retries in the background", err)
			}

			// Its operations fail quietly instead of blocking.
			start := time.Now()
			if _, err := d.SetNX(ctx, "k", "v", time.Minute); err == nil {
				t.Error("SetNX against an unreachable database should error so the store falls back")
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("SetNX took %s, want it bounded by the kv timeout", took)
			}

			closed := make(chan error, 1)
			go func() { closed <- d.Close(ctx) }()
			select {
			case err := <-closed:
				if err != nil {
					t.Errorf("Close = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close did not stop the background setup")
			}

			if _, err := Open(ctx, url, nil, ReadOnly()); err == nil {
				t.Error("Open (read-only) should fail fast for a tool with no database")
			}
		})
	}
}
