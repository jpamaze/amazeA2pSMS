// Package health serves liveness and readiness endpoints.
//
// Readiness checks every dependency a service uses (blueprint rule 10:
// health checks test each dependency, not just the process).
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// CheckFunc reports whether one dependency is usable.
type CheckFunc func(ctx context.Context) error

// Registry holds named readiness checks.
type Registry struct {
	mu     sync.RWMutex
	checks map[string]CheckFunc
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{checks: map[string]CheckFunc{}}
}

// Add registers a readiness check under name.
func (r *Registry) Add(name string, fn CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = fn
}

// Remove drops the check registered under name.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.checks, name)
}

// Result is the outcome of one check.
type Result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Run executes all checks in parallel with a shared timeout.
func (r *Registry) Run(ctx context.Context) (map[string]Result, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out = make(map[string]Result, len(r.checks))
		ok  = true
	)
	for name, fn := range r.checks {
		wg.Add(1)
		go func(name string, fn CheckFunc) {
			defer wg.Done()
			res := Result{OK: true}
			if err := fn(ctx); err != nil {
				res = Result{OK: false, Error: err.Error()}
			}
			mu.Lock()
			out[name] = res
			if !res.OK {
				ok = false
			}
			mu.Unlock()
		}(name, fn)
	}
	wg.Wait()
	return out, ok
}

// LiveHandler answers 200 while the process is running.
func LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// ReadyHandler answers 200 only when every check passes, 503 otherwise.
func (r *Registry) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		results, ok := r.Run(req.Context())
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(results)
	})
}
