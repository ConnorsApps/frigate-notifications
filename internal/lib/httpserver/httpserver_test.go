package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// freeAddr is a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func set(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func waitUp(t *testing.T, addr string) {
	t.Helper()
	for range 100 {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start")
}

// On shutdown a request already in flight must finish, and RunGraceful must
// not return before it has: the caller exits as soon as it does.
func TestShutdownWaitsForInFlightRequests(t *testing.T) {
	addr := freeAddr(t)
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond) // a phone still downloading a clip
		io.WriteString(w, "whole clip")
	})

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { RunGraceful(ctx, addr, h); close(returned) }()
	waitUp(t, addr)

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr)
		if err != nil {
			body <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-started
	cancel()

	select {
	case <-returned:
		select {
		case got := <-body:
			if got != "whole clip" {
				t.Fatalf("in-flight response = %q, want it completed", got)
			}
		default:
			t.Fatal("RunGraceful returned while a request was still being served")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunGraceful never returned")
	}
}

// A request that never finishes must not hang the exit forever.
func TestShutdownGivesUpOnStuckRequests(t *testing.T) {
	set(t, &shutdownTimeout, 200*time.Millisecond)
	addr := freeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- RunGraceful(ctx, addr, h) }()
	waitUp(t, addr)
	go http.Get("http://" + addr)
	<-started
	cancel()

	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("RunGraceful = %v, want nil after giving up on the stuck request", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck request held shutdown past its timeout")
	}
}

// The public listener must drop a connection that never finishes its request
// header, or a few such connections can hold it open indefinitely.
func TestSlowHeaderConnectionIsDropped(t *testing.T) {
	set(t, &readHeaderTimeout, 200*time.Millisecond)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunGraceful(ctx, addr, http.NotFoundHandler())
	waitUp(t, addr)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET /m/snapshot/x.jpg HTTP/1.1\r\nHost: x\r\n") // never finished

	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("the server kept an unfinished request open past its header timeout: %v", err)
	}
}

func TestServeErrorIsReturned(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := RunGraceful(context.Background(), l.Addr().String(), http.NotFoundHandler()); err == nil {
		t.Error("an address already in use must be returned as an error")
	}
}
