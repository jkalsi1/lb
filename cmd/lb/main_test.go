package main

import (
	"net/url"
	"sync/atomic"
	"testing"
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
