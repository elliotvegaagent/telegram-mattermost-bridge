package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/elliotvegaagent/telegram-mattermost-bridge/internal/storage"
)

type adapterStatus struct {
	Healthy     bool
	LastSuccess time.Time
}
type State struct {
	mu       sync.RWMutex
	adapters map[string]adapterStatus
}

func New() *State {
	return &State{adapters: map[string]adapterStatus{"telegram": {}, "mattermost": {}}}
}
func (s *State) Callback(name string) func(bool) {
	return func(healthy bool) {
		s.mu.Lock()
		value := s.adapters[name]
		value.Healthy = healthy
		if healthy {
			value.LastSuccess = time.Now()
		}
		s.adapters[name] = value
		s.mu.Unlock()
	}
}
func (s *State) statuses() (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.adapters["telegram"].Healthy, s.adapters["mattermost"].Healthy
}

func Handler(state *State, store *storage.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		tg, mm := state.statuses()
		age, ageErr := store.OldestPendingAge(r.Context())
		counts, countErr := store.Counts(r.Context())
		ready := tg && mm && ageErr == nil && countErr == nil && (age == nil || *age <= 600)
		status, code := "ready", http.StatusOK
		if !ready {
			status, code = "degraded", http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"status": status, "telegram": tg, "mattermost": mm, "oldest_pending_seconds": age, "outbox": counts})
	})
	return mux
}

func RunServer(ctx context.Context, bind string, handler http.Handler) error {
	server := &http.Server{Addr: bind, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	errors := make(chan error, 1)
	go func() { errors <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errors:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
