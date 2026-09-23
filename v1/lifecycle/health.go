// Package lifecycle handles startup, health endpoints, signals and the
// shutdown sequence.
package lifecycle

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
)

// Health answers three different questions, which is the whole point of the
// type. Conflating them is one of the most reliable ways to turn a partial
// degradation into a total outage.
//
//	/healthz  (liveness)  Is this process wedged and in need of a restart?
//	/readyz   (readiness) Should this instance receive traffic right now?
//	/startupz (startup)   Has initialization finished?
//
// **None of them check dependencies.** That is the load-bearing rule.
//
// If /readyz checks the database, a ten-second database blip marks *every*
// replica unready simultaneously. The load balancer removes all of them,
// and a partial degradation — where the service could have served cached
// reads and returned clean errors for the rest — becomes a total outage.
// Worse, it cannot recover: there are no healthy endpoints left to route
// recovery traffic to.
//
// Readiness answers "can *this instance* serve?", not "is the whole system
// healthy?". Legitimate readiness signals are: initialization finished, not
// draining, and local resources available *for this instance* — a connection
// pool this process has exhausted, not a database everybody shares.
type Health struct {
	live    atomic.Bool
	ready   atomic.Bool
	started atomic.Bool

	mu sync.RWMutex
	// localChecks are per-instance conditions, which is the only kind of
	// check readiness may consult. The name is deliberately awkward so that
	// adding a database ping here looks wrong at the call site.
	localChecks map[string]func() error
}

// NewHealth returns a Health with liveness on and readiness off.
//
// Liveness starts true because the process is, by definition, alive enough
// to run this code; readiness starts false because the listener is not up
// yet, and flipping readiness before the listener accepts sends traffic to
// a connection refused.
func NewHealth() *Health {
	h := &Health{localChecks: make(map[string]func() error)}
	h.live.Store(true)
	return h
}

// SetReady flips readiness.
func (h *Health) SetReady(ready bool) { h.ready.Store(ready) }

// SetStarted marks initialization complete.
func (h *Health) SetStarted(started bool) { h.started.Store(started) }

// SetLive flips liveness. Use this only for genuinely unrecoverable
// process-local state — a corrupted in-memory structure, a background loop
// that has died and cannot be restarted. A dependency being down is not a
// reason to ask for a restart: restarting will not fix it, and a crash loop
// across every replica turns a dependency blip into an outage of your own.
func (h *Health) SetLive(live bool) { h.live.Store(live) }

// AddLocalCheck registers a readiness check about *this instance*.
//
// The documented rule is that a check registered here must not perform I/O
// against a shared dependency. Checking whether this process's own
// connection pool is exhausted is fine; pinging the database is not.
func (h *Health) AddLocalCheck(name string, check func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.localChecks[name] = check
}

// Ready reports readiness and the reason when not ready.
func (h *Health) Ready() (bool, map[string]string) {
	if !h.ready.Load() {
		return false, map[string]string{"state": "not accepting traffic"}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()

	failures := map[string]string{}
	for name, check := range h.localChecks {
		if err := check(); err != nil {
			failures[name] = err.Error()
		}
	}
	if len(failures) > 0 {
		return false, failures
	}
	return true, nil
}

// Live reports liveness.
func (h *Health) Live() bool { return h.live.Load() }

// Started reports whether initialization finished.
func (h *Health) Started() bool { return h.started.Load() }

// Handler returns the admin HTTP handlers for the three endpoints.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !h.Live() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, failures := h.Ready()
		if !ready {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"status": "not ready", "checks": failures})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
	})

	mux.HandleFunc("GET /startupz", func(w http.ResponseWriter, r *http.Request) {
		if !h.Started() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "started"})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
