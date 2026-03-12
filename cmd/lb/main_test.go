package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func makeBackend(rawURL string, alive bool, conns int64) *Backend {
	u, _ := url.Parse(rawURL)
	b := &Backend{URL: u, Alive: alive}
	atomic.StoreInt64(&b.NumConnections, conns)
	return b
}

func TestGetLeastConnectedPeer_PicksLowest(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", true, 5))
	pool.AddBackend(makeBackend("http://b:8081", true, 2)) // should win

	peer := pool.GetLeastConnectedPeer()
	if peer.URL.Host != "b:8081" {
		t.Errorf("expected b:8081, got %s", peer.URL.Host)
	}
}

func TestGetLeastConnectedPeer_SkipsDeadBackends(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", false, 0)) // dead with 0 conns
	pool.AddBackend(makeBackend("http://b:8081", true, 6))
	pool.AddBackend(makeBackend("http://b:8082", true, 5)) // should win

	peer := pool.GetLeastConnectedPeer()
	if peer.URL.Host != "b:8082" {
		t.Errorf("expected b:8082, got %s", peer.URL.Host)
	}
}

func TestGetLeastConnectedPeer_AllDead(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", false, 0))
	pool.AddBackend(makeBackend("http://b:8081", false, 0))

	peer := pool.GetLeastConnectedPeer()
	if peer != nil {
		t.Errorf("expected nil, got %s", peer.URL.Host)
	}
}

func TestGetLeastConnectedPeer_TieBreaksFirst(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", true, 3)) // should win (first seen)
	pool.AddBackend(makeBackend("http://b:8081", true, 3))

	peer := pool.GetLeastConnectedPeer()
	if peer.URL.Host != "a:8080" {
		t.Errorf("expected a:8080 on tie, got %s", peer.URL.Host)
	}
}

func TestGetLeastConnectedPeer_ZeroConnections(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", true, 1))
	pool.AddBackend(makeBackend("http://b:8081", true, 0)) // should win

	peer := pool.GetLeastConnectedPeer()
	if peer.URL.Host != "b:8081" {
		t.Errorf("expected b:8081, got %s", peer.URL.Host)
	}
}

func TestGetNextPeer_ReturnsAlive(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", true, 0))

	peer := pool.GetNextPeer()
	if peer == nil {
		t.Fatal("expected a peer, got nil")
	}
	if peer.URL.Host != "a:8080" {
		t.Errorf("expected a:8080, got %s", peer.URL.Host)
	}
}

func TestGetNextPeer_AllDead(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", false, 0))
	pool.AddBackend(makeBackend("http://b:8081", false, 0))

	peer := pool.GetNextPeer()
	if peer != nil {
		t.Errorf("expected nil, got %s", peer.URL.Host)
	}
}

func TestGetNextPeer_SkipsDeadBackends(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", false, 0)) // dead
	pool.AddBackend(makeBackend("http://b:8081", true, 0))  // alive

	peer := pool.GetNextPeer()
	if peer == nil {
		t.Fatal("expected a peer, got nil")
	}
	if peer.URL.Host != "b:8081" {
		t.Errorf("expected b:8081, got %s", peer.URL.Host)
	}
}

func TestGetNextPeer_RoundRobin(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", true, 0))
	pool.AddBackend(makeBackend("http://b:8081", true, 0))
	pool.AddBackend(makeBackend("http://c:8082", true, 0))

	// NextIndex increments current before returning, so first call yields index 1
	got := make([]string, 3)
	for i := range got {
		p := pool.GetNextPeer()
		if p == nil {
			t.Fatalf("call %d: expected a peer, got nil", i)
		}
		got[i] = p.URL.Host
	}

	// All three backends must appear exactly once across the three calls
	seen := map[string]int{}
	for _, h := range got {
		seen[h]++
	}
	for _, host := range []string{"a:8080", "b:8081", "c:8082"} {
		if seen[host] != 1 {
			t.Errorf("expected %s exactly once in round, got %d (sequence: %v)", host, seen[host], got)
		}
	}
}

func TestGetNextPeer_WrapsAround(t *testing.T) {
	pool := &ServerPool{}
	pool.AddBackend(makeBackend("http://a:8080", false, 0)) // dead
	pool.AddBackend(makeBackend("http://b:8081", false, 0)) // dead
	pool.AddBackend(makeBackend("http://c:8082", true, 0))  // alive — only one

	// Regardless of where the round-robin pointer starts, we should always
	// wrap around and find c:8082.
	for i := 0; i < 6; i++ {
		peer := pool.GetNextPeer()
		if peer == nil {
			t.Fatalf("call %d: expected c:8082, got nil", i)
		}
		if peer.URL.Host != "c:8082" {
			t.Errorf("call %d: expected c:8082, got %s", i, peer.URL.Host)
		}
	}
}

// --- Strategy tests ---

func TestRoundRobinStrategy(t *testing.T) {
	backends := []*Backend{
		makeBackend("http://a:8080", true, 0),
		makeBackend("http://b:8081", true, 0),
		makeBackend("http://c:8082", true, 0),
	}
	rr := &RoundRobin{}

	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		p := rr.Next(backends)
		if p == nil {
			t.Fatalf("call %d: expected a backend, got nil", i)
		}
		seen[p.URL.Host]++
	}
	for _, host := range []string{"a:8080", "b:8081", "c:8082"} {
		if seen[host] != 1 {
			t.Errorf("expected %s exactly once, got %d (seen: %v)", host, seen[host], seen)
		}
	}
}

func TestLeastConnectionsStrategy(t *testing.T) {
	backends := []*Backend{
		makeBackend("http://a:8080", true, 5),
		makeBackend("http://b:8081", true, 1), // should win
		makeBackend("http://c:8082", true, 3),
	}
	lc := &LeastConnections{}
	p := lc.Next(backends)
	if p == nil {
		t.Fatal("expected a backend, got nil")
	}
	if p.URL.Host != "b:8081" {
		t.Errorf("expected b:8081, got %s", p.URL.Host)
	}
}

func TestStrategySkipsDeadBackends(t *testing.T) {
	backends := []*Backend{
		makeBackend("http://a:8080", false, 0), // dead
		makeBackend("http://b:8081", true, 0),  // alive
	}

	rr := &RoundRobin{}
	for i := 0; i < 6; i++ {
		p := rr.Next(backends)
		if p != nil && p.URL.Host == "a:8080" {
			t.Errorf("RoundRobin returned dead backend on call %d", i)
		}
	}

	lc := &LeastConnections{}
	for i := 0; i < 3; i++ {
		p := lc.Next(backends)
		if p != nil && p.URL.Host == "a:8080" {
			t.Errorf("LeastConnections returned dead backend on call %d", i)
		}
	}
}

// --- Graceful shutdown test ---

func TestGracefulShutdown(t *testing.T) {
	// Slow handler simulates in-flight work.
	const handlerDelay = 150 * time.Millisecond
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(handlerDelay)
		w.WriteHeader(http.StatusOK)
	})

	// Bind on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	server := &http.Server{Handler: handler}

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		server.Serve(ln) //nolint:errcheck — ErrServerClosed is expected
	}()

	// Fire a request that will still be in-flight when we shut down.
	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr)
		if err != nil {
			reqDone <- err
			return
		}
		resp.Body.Close()
		reqDone <- nil
	}()

	// Wait just long enough for the request to reach the handler.
	time.Sleep(20 * time.Millisecond)

	// Initiate graceful shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	// The in-flight request must have completed successfully.
	if err := <-reqDone; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}

	// The server goroutine should have exited.
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server goroutine did not exit after Shutdown")
	}

	// httptest.NewServer is used here purely to confirm our handler works standalone.
	ts := httptest.NewServer(handler)
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("httptest request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}
