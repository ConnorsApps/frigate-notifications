package store

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRedis answers just enough RESP for go-redis: PING, the connection
// handshake, and SET [EX n] [NX], counting the SETs it received.
type fakeRedis struct {
	l    net.Listener
	sets atomic.Int32
	seen map[string]bool
}

func serveFakeRedis(t *testing.T, addr string) *fakeRedis {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{l: l, seen: map[string]bool{}}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "PING":
			io.WriteString(c, "+PONG\r\n")
		case "SET":
			f.sets.Add(1)
			nx := false
			for _, a := range args[3:] {
				nx = nx || strings.EqualFold(a, "NX")
			}
			if nx && f.seen[args[1]] {
				io.WriteString(c, "$-1\r\n")
				continue
			}
			f.seen[args[1]] = true
			io.WriteString(c, "+OK\r\n")
		case "HELLO":
			io.WriteString(c, "-ERR unknown command 'HELLO'\r\n")
		default: // CLIENT SETINFO and friends
			io.WriteString(c, "+OK\r\n")
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, fmt.Errorf("bad array header %q", line)
	}
	args := make([]string, n)
	for i := range args {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

// Valkey is deployed beside the app and can come up after it. Failing the
// first ping must not leave the process on in-memory state for good.
func TestRedisThatComesUpAfterStartupIsUsed(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close() // nothing listens yet

	s := New(context.Background(), "redis://"+addr, nil)
	t.Cleanup(func() { s.Close() })
	if s.primary == nil {
		t.Fatal("the Valkey backend was dropped because its first ping failed")
	}

	// While it is down every operation still works, from memory.
	if !s.AcquireCooldown(context.Background(), "down", time.Minute) {
		t.Fatal("a first cooldown must be granted from the in-memory fallback")
	}

	server := serveFakeRedis(t, addr)
	// go-redis may need a moment to notice; a failed attempt falls back, so
	// poll until an operation reaches the server.
	deadline := time.Now().Add(5 * time.Second)
	for server.sets.Load() == 0 && time.Now().Before(deadline) {
		s.AcquireCooldown(context.Background(), "up", time.Minute)
		time.Sleep(50 * time.Millisecond)
	}
	if server.sets.Load() == 0 {
		t.Fatal("state never reached Valkey after it came up")
	}
}

func TestInvalidRedisURLFallsBackToMemory(t *testing.T) {
	s := New(context.Background(), "http://not-redis", nil)
	if s.primary != nil {
		t.Error("an unparseable url should leave the store on memory")
	}
	if !s.AcquireCooldown(context.Background(), "k", time.Minute) {
		t.Error("the store must keep working")
	}
}
