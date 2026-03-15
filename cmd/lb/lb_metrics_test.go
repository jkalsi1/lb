package main

// Note: helpers here are named newTestBackend/newPool (not makeBackend/makePool)
// to avoid a symbol collision with the identically-named helpers in main_test.go.
//
// All of Backend, ServerPool, Strategy, RoundRobin, LeastConnections are already
// exported from main.go — no changes to main.go were required.
//
// lbHandlerFor mirrors the connection-tracking logic in lb() but against a
// caller-supplied *ServerPool instead of the package-level global, so each test
// is fully isolated.

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestBackend starts an httptest.Server with handler, registers t.Cleanup
// to close it, and returns a live *Backend wired with a reverse proxy.
func newTestBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("newTestBackend: url.Parse: %v", err)
	}
	return &Backend{
		URL:          u,
		State:        Closed,
		ReverseProxy: httputil.NewSingleHostReverseProxy(u),
	}
}

// newPool constructs a *ServerPool with the given strategy and backends.
func newPool(strategy Strategy, backends ...*Backend) *ServerPool {
	pool := &ServerPool{strategy: strategy}
	for _, b := range backends {
		pool.AddBackend(b)
	}
	return pool
}

// lbHandlerFor returns an http.HandlerFunc that load-balances through pool,
// mirroring the acquire/release connection-counting logic in lb().
func lbHandlerFor(pool *ServerPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer := pool.strategy.Next(pool.backends)
		if peer == nil {
			http.Error(w, "Service not available", http.StatusServiceUnavailable)
			return
		}
		atomic.AddInt64(&peer.NumConnections, 1)
		peer.ReverseProxy.ServeHTTP(w, r)
		atomic.AddInt64(&peer.NumConnections, -1)
	}
}

// ── Test 1: round-robin distributes evenly ───────────────────────────────────

func TestRoundRobinDistribution(t *testing.T) {
	const (
		nBackends = 3
		nRequests = 999
		want      = nRequests / nBackends
		tolerance = 5
	)

	counts := make([]int64, nBackends)
	bs := make([]*Backend, nBackends)
	for i := range bs {
		i := i // capture
		bs[i] = newTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&counts[i], 1)
			w.WriteHeader(http.StatusOK)
		}))
	}

	handler := lbHandlerFor(newPool(&RoundRobin{}, bs...))

	for i := 0; i < nRequests; i++ {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: unexpected status %d", i, w.Code)
		}
	}

	t.Log("RoundRobin distribution:")
	for i, c := range counts {
		got := int(atomic.LoadInt64(&c))
		t.Logf("  backend[%d]: %d requests", i, got)
		if diff := abs(got - want); diff > tolerance {
			t.Errorf("backend[%d] got %d requests, want %d ±%d", i, got, want, tolerance)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ── Test 2: least-connections routes away from the slow backend ───────────────

// TestLeastConnectionsRoutesToFastestBackend verifies that LeastConnections
// steers traffic away from a slow backend that accumulates in-flight connections.
//
// The naive approach (60 goroutines firing simultaneously) doesn't work: they
// all start within nanoseconds, before any 5 ms B/C request completes, so the
// scheduler sees equal counts and gives a perfect 20/20/20 split.
//
// Fix: pre-warm A with real in-flight connections using a single-backend pool,
// then spin-wait until NumConnections ≥ prewarm.  Now every test goroutine
// observes A loaded and prefers B/C instead.
func TestLeastConnectionsRoutesToFastestBackend(t *testing.T) {
	const (
		nGoroutines = 60
		prewarm     = 10 // long-running requests to hold open on A
	)

	var countA, countB, countC int64

	// A is slow (200 ms); B and C are fast (5 ms).
	backendA := newTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&countA, 1)
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	backendB := newTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&countB, 1)
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	backendC := newTestBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&countC, 1)
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))

	// Route `prewarm` requests exclusively to A to load it up.
	prewarmHandler := lbHandlerFor(newPool(&LeastConnections{}, backendA))
	for i := 0; i < prewarm; i++ {
		go prewarmHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}

	// Spin-wait until A's NumConnections reflects real in-flight requests.
	// lbHandlerFor increments the counter before ServeHTTP blocks, so this is
	// visible as soon as the goroutine is scheduled — typically < 5 ms.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&backendA.NumConnections) < int64(prewarm) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for pre-warm connections to establish")
		}
		time.Sleep(time.Millisecond)
	}

	// Reset counters: measure only the 60 test goroutines below.
	atomic.StoreInt64(&countA, 0)
	atomic.StoreInt64(&countB, 0)
	atomic.StoreInt64(&countC, 0)

	// With A carrying ≥10 connections and B/C at 0, LeastConnections routes
	// the first ~20 goroutines to B/C.  After B/C also reach 10, every 3rd
	// goroutine goes to A — giving A roughly ⌊40/3⌋ ≈ 13 test requests.
	fullHandler := lbHandlerFor(newPool(&LeastConnections{}, backendA, backendB, backendC))

	var wg sync.WaitGroup
	wg.Add(nGoroutines)
	for i := 0; i < nGoroutines; i++ {
		go func() {
			defer wg.Done()
			fullHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	wg.Wait()

	a := atomic.LoadInt64(&countA)
	b := atomic.LoadInt64(&countB)
	c := atomic.LoadInt64(&countC)
	t.Logf("LeastConnections distribution: A(200ms)=%d  B(5ms)=%d  C(5ms)=%d  total=%d",
		a, b, c, a+b+c)

	if a >= 15 {
		t.Errorf("slow backend A got %d requests (≥15); LeastConnections should deprioritize it", a)
	}
	if b+c <= 45 {
		t.Errorf("fast backends B+C got %d combined (≤45); expected > 45", b+c)
	}
}

// ── Test 3: health-check recovery ────────────────────────────────────────────

func TestHealthCheckRecovery(t *testing.T) {
	var countA, countB int64

	handlerA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&countA, 1)
		w.WriteHeader(http.StatusOK)
	})
	handlerB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&countB, 1)
		w.WriteHeader(http.StatusOK)
	})

	// backendB has auto-cleanup. backendA is managed manually so we can close
	// and replace the underlying server to simulate failure and recovery.
	backendB := newTestBackend(t, handlerB)

	tsA := httptest.NewServer(handlerA)
	uA, _ := url.Parse(tsA.URL)
	backendA := &Backend{
		URL:          uA,
		State:        Closed,
		ReverseProxy: httputil.NewSingleHostReverseProxy(uA),
	}

	pool := newPool(&RoundRobin{}, backendA, backendB)
	handler := lbHandlerFor(pool)

	send := func(n int) {
		for i := 0; i < n; i++ {
			handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}
	}
	reset := func() { atomic.StoreInt64(&countA, 0); atomic.StoreInt64(&countB, 0) }

	// ── Phase 1: both alive ──────────────────────────────────────────────────
	send(20)
	a1, b1 := atomic.LoadInt64(&countA), atomic.LoadInt64(&countB)
	t.Logf("Phase 1 (both alive):    A=%d  B=%d", a1, b1)
	if a1 == 0 || b1 == 0 {
		t.Errorf("Phase 1: both backends should receive traffic, got A=%d B=%d", a1, b1)
	}

	// ── Phase 2: A dies ──────────────────────────────────────────────────────
	tsA.Close()
	pool.HealthCheck() // isBackendAlive dials tsA's port → connection refused → marks dead
	reset()

	send(20)
	a2, b2 := atomic.LoadInt64(&countA), atomic.LoadInt64(&countB)
	t.Logf("Phase 2 (A dead):        A=%d  B=%d", a2, b2)
	if a2 != 0 {
		t.Errorf("Phase 2: dead backend A should get 0 requests, got %d", a2)
	}
	if b2 != 20 {
		t.Errorf("Phase 2: all 20 requests should reach B, got %d", b2)
	}

	// ── Phase 3: A recovers on a new port ────────────────────────────────────
	tsA2 := httptest.NewServer(handlerA)
	t.Cleanup(tsA2.Close)
	uA2, _ := url.Parse(tsA2.URL)
	backendA.URL = uA2
	backendA.ReverseProxy = httputil.NewSingleHostReverseProxy(uA2)

	pool.HealthCheck() // new address dials OK → marks alive
	reset()

	send(20)
	a3, b3 := atomic.LoadInt64(&countA), atomic.LoadInt64(&countB)
	t.Logf("Phase 3 (A recovered):   A=%d  B=%d", a3, b3)
	if a3 == 0 || b3 == 0 {
		t.Errorf("Phase 3: both backends should receive traffic after recovery, got A=%d B=%d", a3, b3)
	}
}

// ── Test 4: latency percentiles ───────────────────────────────────────────────

func TestLatencyPercentiles(t *testing.T) {
	const nRequests = 1000

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	latencyHandler := func() http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ms := 1 + rng.Intn(20) // uniform [1, 20] ms backend latency
			time.Sleep(time.Duration(ms) * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		})
	}

	pool := newPool(
		&RoundRobin{},
		newTestBackend(t, latencyHandler()),
		newTestBackend(t, latencyHandler()),
	)
	handler := lbHandlerFor(pool)

	latencies := make([]time.Duration, 0, nRequests)
	for i := 0; i < nRequests; i++ {
		start := time.Now()
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		latencies = append(latencies, time.Since(start))
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	p50 := latencies[nRequests*50/100]
	p95 := latencies[nRequests*95/100]
	p99 := latencies[nRequests*99/100]

	t.Logf("Latency percentiles over %d sequential requests:", nRequests)
	t.Logf("  p50 = %v", p50)
	t.Logf("  p95 = %v", p95)
	t.Logf("  p99 = %v", p99)

	const maxP99 = 100 * time.Millisecond
	if p99 > maxP99 {
		t.Errorf("p99 %v exceeds %v — LB is adding unexpected overhead", p99, maxP99)
	}
}

// ── Benchmark: throughput ─────────────────────────────────────────────────────

// Run: go test -bench=BenchmarkLBThroughput -benchtime=10s
//
// Uses a nopTransport so the reverse proxy returns immediately without making
// real TCP connections.  This isolates pure LB overhead: strategy.Next() +
// two atomic counter ops + httputil.ReverseProxy header copying.
// Real-network throughput (with a localhost httptest backend) is ~4-5k req/sec;
// this measures the ceiling of what the routing layer itself can sustain.

// nopTransport answers every proxied request instantly with 200 OK and an
// empty body, without opening any network connection.
type nopTransport struct{}

func (nopTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     make(http.Header),
	}, nil
}

// nopBackend creates a *Backend whose ReverseProxy uses nopTransport.
func nopBackend(host string) *Backend {
	u := &url.URL{Scheme: "http", Host: host}
	proxy := &httputil.ReverseProxy{
		Director:  func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = host },
		Transport: nopTransport{},
	}
	return &Backend{URL: u, State: Closed, ReverseProxy: proxy}
}

func BenchmarkLBThroughput(b *testing.B) {
	pool := newPool(&RoundRobin{},
		nopBackend("bench-a"),
		nopBackend("bench-b"),
		nopBackend("bench-c"),
	)
	handler := lbHandlerFor(pool)

	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}
	})

	b.StopTimer()
	elapsed := time.Since(start)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "req/sec")
}
