package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var FAILURE_THRESHOLD = 20

const FAILURE_TIMEOUT = (5 * time.Second)

const (
	Attempts int = iota
	Retry
)

type CircuitState int

// CircuitState enum
const (
	Closed   CircuitState = iota // good to send requests
	Open                         // don't send requests
	HalfOpen                     // send ONE request (probe)
)

// Backend holds the data about a server
type Backend struct {
	URL              *url.URL
	Mutex            sync.Mutex
	ReverseProxy     *httputil.ReverseProxy
	NumConnections   int64
	State            CircuitState
	FailureCount     int64
	OpenUntil        time.Time
	HalfOpenInFlight bool
}

// RecordOneFailure increments failure count for a backend.
// If threshold exceeded, trips circuit and sets openUntil.
func (b *Backend) RecordOneFailure() {
	b.Mutex.Lock()
	defer b.Mutex.Unlock()
	b.FailureCount++
	if b.FailureCount >= int64(FAILURE_THRESHOLD) {
		b.State = Open
		b.OpenUntil = time.Now().Add(FAILURE_TIMEOUT)
		b.FailureCount = 0
	}
}

// RecordSuccess resets failureCount to 0 and sets circuit state to Closed

func (b *Backend) RecordSuccess() {
	b.Mutex.Lock()
	defer b.Mutex.Unlock()
	b.FailureCount = 0
	if b.State == HalfOpen {
		b.State = Closed
	}
}

// SetAlive for this backend
func (b *Backend) SetAlive(state CircuitState) {
	b.Mutex.Lock()
	b.State = state
	b.Mutex.Unlock()
}

// IsAlive returns true when backend is alive
func (b *Backend) IsAlive() (alive bool) {
	b.Mutex.Lock()
	switch b.State {
	case Closed:
		alive = true
	case Open:
		if time.Now().After(b.OpenUntil) {
			alive = true
			b.State = HalfOpen
		} else {
			alive = false
		}
	case HalfOpen:
		alive = !b.HalfOpenInFlight
	}
	b.Mutex.Unlock()
	return
}

// Strategy selects the next backend from a pool.
type Strategy interface {
	Next(backends []*Backend) *Backend
}

// RoundRobin cycles through backends in order, skipping dead ones.
type RoundRobin struct {
	current uint64
}

func (rr *RoundRobin) Next(backends []*Backend) *Backend {
	next := int(atomic.AddUint64(&rr.current, uint64(1)) % uint64(len(backends)))
	l := len(backends) + next
	for i := next; i < l; i++ {
		idx := i % len(backends)
		if backends[idx].IsAlive() {
			if i != next {
				atomic.StoreUint64(&rr.current, uint64(idx))
			}
			return backends[idx]
		}
	}
	return nil
}

// LeastConnections picks the alive backend with the fewest active connections.
type LeastConnections struct{}

func (lc *LeastConnections) Next(backends []*Backend) *Backend {
	var best *Backend
	for _, b := range backends {
		if !b.IsAlive() {
			continue
		}
		if best == nil || b.NumConnections < best.NumConnections {
			best = b
		}
	}
	return best
}

// ServerPool holds information about reachable backends
type ServerPool struct {
	backends []*Backend
	current  uint64
	strategy Strategy
}

// AddBackend to the server pool
func (s *ServerPool) AddBackend(backend *Backend) {
	s.backends = append(s.backends, backend)
}

// NextIndex atomically increase the counter and return an index
func (s *ServerPool) NextIndex() int {
	return int(atomic.AddUint64(&s.current, uint64(1)) % uint64(len(s.backends)))
}

// MarkBackendStatus changes a status of a backend
func (s *ServerPool) MarkBackendStatus(backendUrl *url.URL, state CircuitState) {
	for _, b := range s.backends {
		if b.URL.String() == backendUrl.String() {
			if state == Open {
				b.RecordOneFailure()
			} else {
				b.RecordSuccess()
			}
			break
		}
	}
}

// SubtractOneBackendConnection reduces num connections of one backend
func (s *ServerPool) SubtractOneBackendConnection(backendUrl *url.URL) {
	for _, b := range s.backends {
		if b.URL.String() == backendUrl.String() {
			atomic.AddInt64(&b.NumConnections, int64(-1))
			break
		}
	}
}

// GetNextPeer returns next active peer to take a connection
func (s *ServerPool) GetNextPeer() *Backend {
	// loop entire backends to find out an Alive backend
	next := s.NextIndex()
	l := len(s.backends) + next // start from next and move a full cycle
	for i := next; i < l; i++ {
		idx := i % len(s.backends)     // take an index by modding
		if s.backends[idx].IsAlive() { // if we have an alive backend, use it and store if its not the original one
			if i != next {
				atomic.StoreUint64(&s.current, uint64(idx))
			}
			return s.backends[idx]
		}
	}
	return nil
}

// GetLeastConnectedPeer returns peer w/least connections
func (s *ServerPool) GetLeastConnectedPeer() *Backend {
	var best *Backend
	for _, b := range s.backends {
		if !b.IsAlive() {
			continue
		}
		// pick up the first element we see as a backend, and if we see a better one, choose it
		if best == nil || b.NumConnections < best.NumConnections {
			best = b
		}
	}
	return best
}

// HealthCheck pings the backends and updates circuit state accordingly.
func (s *ServerPool) HealthCheck() {
	for _, b := range s.backends {
		reachable := isBackendAlive(b.URL)
		b.Mutex.Lock()
		if reachable == Closed {
			b.State = Closed
			b.FailureCount = 0
		} else {
			b.State = Open
			b.OpenUntil = time.Now().Add(FAILURE_TIMEOUT)
		}
		status := "up"
		if b.State == Open {
			status = "down"
		}
		b.Mutex.Unlock()
		log.Printf("%s [%s]\n", b.URL, status)
	}
}

// GetAttemptsFromContext returns the attempts for request
func GetAttemptsFromContext(r *http.Request) int {
	if attempts, ok := r.Context().Value(Attempts).(int); ok {
		return attempts
	}
	return 1
}

// GetRetryFromContext returns the retries for request
func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(Retry).(int); ok {
		return retry
	}
	return 0
}

// logConnectionSnapshot prints current NumConnections for every backend
func (s *ServerPool) logConnectionSnapshot(label string) {
	parts := make([]string, len(s.backends))
	for i, b := range s.backends {
		parts[i] = fmt.Sprintf("%s=%d", b.URL.Host, atomic.LoadInt64(&b.NumConnections))
	}
	log.Printf("[connections] %s | %s", label, strings.Join(parts, "  "))
}

// lb load balances the incoming request
func lb(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttemptsFromContext(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached, terminating\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}

	peer := serverPool.strategy.Next(serverPool.backends)
	if peer != nil {
		// If this backend is in HalfOpen state, mark the probe as in-flight so
		// IsAlive() blocks additional requests until the probe completes.
		peer.Mutex.Lock()
		isProbe := peer.State == HalfOpen
		if isProbe {
			peer.HalfOpenInFlight = true
		}
		peer.Mutex.Unlock()

		conns := atomic.AddInt64(&peer.NumConnections, int64(1))
		log.Printf("[acquire] %s  active=%d", peer.URL.Host, conns)
		serverPool.logConnectionSnapshot("after acquire")

		peer.ReverseProxy.ServeHTTP(w, r)

		if isProbe {
			peer.Mutex.Lock()
			peer.HalfOpenInFlight = false
			peer.Mutex.Unlock()
		}

		conns = atomic.AddInt64(&peer.NumConnections, int64(-1))
		log.Printf("[release] %s  active=%d", peer.URL.Host, conns)
		serverPool.logConnectionSnapshot("after release")
		return
	}
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

// isAlive checks whether a backend is Alive by establishing a TCP connection
func isBackendAlive(u *url.URL) CircuitState {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		log.Println("Site unreachable, error: ", err)
		return Open
	}
	defer conn.Close()
	return Closed
}

// healthCheck runs a routine for check status of the backends every 2 mins.
// It stops when ctx is cancelled.
func healthCheck(ctx context.Context) {
	t := time.NewTicker(time.Minute * 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Println("Starting health check...")
			serverPool.HealthCheck()
			log.Println("Health check completed")
		}
	}
}

var serverPool ServerPool

func main() {
	var serverList string
	var port int
	var strategyName string
	flag.StringVar(&serverList, "backends", "", "Load balanced backends, use commas to separate")
	flag.IntVar(&port, "port", 3030, "Port to serve")
	flag.StringVar(&strategyName, "strategy", "leastconn", "Load balancing strategy: roundrobin or leastconn")
	flag.Parse()

	if len(serverList) == 0 {
		log.Fatal("Please provide one or more backends to load balance")
	}

	switch strategyName {
	case "roundrobin":
		serverPool.strategy = &RoundRobin{}
	default:
		serverPool.strategy = &LeastConnections{}
	}

	// parse servers
	tokens := strings.Split(serverList, ",")
	for _, tok := range tokens {
		serverUrl, err := url.Parse(tok)
		if err != nil {
			log.Fatal(err)
		}

		// Use a shared transport per backend so idle connections are reused
		// across concurrent requests instead of being closed after each one.
		// Without this, DefaultTransport keeps only 2 idle conns per host,
		// causing ~198 connections to close (TIME_WAIT) after every 200-concurrency
		// wave and quickly exhausting the process's file-descriptor limit.
		transport := &http.Transport{
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 300,
			IdleConnTimeout:     90 * time.Second,
		}

		proxy := httputil.NewSingleHostReverseProxy(serverUrl)
		proxy.Transport = transport
		// ModifyResponse is called on every successful (non-error) response.
		// Record the success so the circuit breaker can close a half-open circuit.
		proxy.ModifyResponse = func(resp *http.Response) error {
			serverPool.MarkBackendStatus(serverUrl, Closed)
			return nil
		}
		proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
			log.Printf("[%s] %s\n", serverUrl.Host, e.Error())
			retries := GetRetryFromContext(request)
			if retries < 3 {
				<-time.After(10 * time.Millisecond) // sleep 10ms
				ctx := context.WithValue(request.Context(), Retry, retries+1)
				proxy.ServeHTTP(writer, request.WithContext(ctx))
				return
			}

			// after 3 retries, mark this backend as down
			serverPool.MarkBackendStatus(serverUrl, Open)

			// if the same request routing for few attempts with different backends, increase the count
			attempts := GetAttemptsFromContext(request)
			log.Printf("%s(%s) Attempting retry %d\n", request.RemoteAddr, request.URL.Path, attempts)
			ctx := context.WithValue(request.Context(), Attempts, attempts+1)
			lb(writer, request.WithContext(ctx))
		}

		serverPool.AddBackend(&Backend{
			URL:          serverUrl,
			State:        Closed,
			ReverseProxy: proxy,
		})
		log.Printf("Configured server: %s\n", serverUrl)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(lb),
	}

	// Derive a context for the health-check goroutine so it stops on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go healthCheck(ctx)

	go func() {
		log.Printf("Load Balancer started at :%d\n", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Println("Shutting down...")
	cancel() // stop health check goroutine

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}
	log.Println("Server exited cleanly")
}
