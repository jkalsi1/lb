# Issue: backends die permanently on transient connection errors

## What's happening

Under load (`hey -z 10s -c 5`), the LB marks backends as permanently dead after a
single request fails its 3 retries. The backend process is still healthy — `curl`
confirms it responds fine — but the LB stops routing to it until the 2-minute health
check fires.

The root cause is in `cmd/lb/main.go` inside the `proxy.ErrorHandler` closure:

```go
proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
    retries := GetRetryFromContext(request)
    if retries < 3 {
        <-time.After(10 * time.Millisecond)
        ctx := context.WithValue(request.Context(), Retry, retries+1)
        proxy.ServeHTTP(writer, request.WithContext(ctx)) // retries same backend
        return
    }
    serverPool.MarkBackendStatus(serverUrl, false) // permanent death
    ...
}
```

Three problems:
1. Retries hit the **same** backend — a struggling backend gets hammered further.
2. A **single request** failing 3 times condemns the backend for everyone.
3. Death is **permanent** until health check (every 2 minutes).

The errors that trigger this (`connection reset by peer`, `broken pipe`) are normal
OS-level noise under high concurrency — stale keep-alive connections, full socket
accept queues. They are not evidence the backend process is down.

---

## Prompt: implement circuit breaking with timed recovery

Modify `cmd/lb/main.go` only. Do not add any new dependencies (stdlib only).

### What to build

Add a **per-backend circuit breaker** with three states:

- **Closed** (normal): requests flow through. On failure, increment a failure counter.
- **Open** (tripped): backend is marked dead. No requests routed to it. After a
  configurable cooldown (default 10s), transition to half-open.
- **Half-open**: allow one probe request through. If it succeeds, reset to closed.
  If it fails, reset the cooldown and go back to open.

### Concrete requirements

1. Add fields to `Backend` (or a new `CircuitBreaker` struct embedded in `Backend`):
   - `failureCount int64` — atomic, counts consecutive failures
   - `openUntil int64` — atomic, Unix nanoseconds; zero means closed
   - `halfOpen int32` — atomic flag (0 or 1), true while a probe is in flight

2. Replace the `serverPool.MarkBackendStatus(serverUrl, false)` call in the error
   handler with a call to a new method `Backend.recordFailure(threshold int, cooldown time.Duration)`:
   - Increment `failureCount` atomically
   - If `failureCount >= threshold`, set `openUntil = time.Now().Add(cooldown).UnixNano()`
     and call `SetAlive(false)`

3. Add `Backend.isOpen() bool` that checks `time.Now().UnixNano() < openUntil`.

4. Update `Backend.IsAlive()` (or the strategy's `Next`) so that when a backend's
   `openUntil` has elapsed, it atomically transitions to half-open: set
   `halfOpen = 1`, call `SetAlive(true)`, and allow one request through.

5. On **success** (in `lb()` after `peer.ReverseProxy.ServeHTTP` returns without
   the error handler firing), call a new `Backend.recordSuccess()` that:
   - Resets `failureCount` to 0
   - Resets `openUntil` to 0
   - Resets `halfOpen` to 0
   - Calls `SetAlive(true)`

6. Add a `-failure-threshold` flag (default: `5`) and `-circuit-cooldown` flag
   (default: `10s`) wired into the error handler call.

7. Remove (or keep but demote) the existing `MarkBackendStatus` call from the
   error handler — circuit state should now be the source of truth for liveness
   during a request path. The 2-minute `HealthCheck` can remain as a background
   recovery mechanism for genuinely dead backends.

### Constraints
- All changes in `cmd/lb/main.go` only
- No new dependencies
- Existing tests must still pass
- Add at least one test in `main_test.go` or a new file: `TestCircuitBreaker_OpensAfterThreshold`
  and `TestCircuitBreaker_HalfOpenRecovery`
