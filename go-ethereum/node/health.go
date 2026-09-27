// Copyright (c) 2024 XDC Network
// Health check HTTP endpoint for GP5 nodes (fix #89).
//
// GET /health returns 200 OK with JSON when the node is in sync and running,
// or 503 Service Unavailable when the node is stalled or not yet started.
//
// Example response (200):
//
//	{
//	  "status": "ok",
//	  "syncing": false,
//	  "peers": 12,
//	  "block": 18000000,
//	  "engine": "XDPoS"
//	}

package node

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// HealthStatus holds the current health snapshot written by the node.
type HealthStatus struct {
	Syncing   bool   `json:"syncing"`
	Peers     int    `json:"peers"`
	Block     uint64 `json:"block"`
	Engine    string `json:"engine"`
	UpdatedAt int64  `json:"updated_at"` // unix seconds
}

// healthState is set atomically; nil means "not yet initialised".
var healthState atomic.Pointer[HealthStatus]

// SetHealthStatus is called by the eth backend on every new head / sync tick.
func SetHealthStatus(s HealthStatus) {
	s.UpdatedAt = time.Now().Unix()
	healthState.Store(&s)
}

// HealthHandler serves GET /health.
// It is registered on the canonical HTTP server via Node.RegisterHandler.
var HealthHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s := healthState.Load()
	if s == nil {
		// Node not yet ready.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
		return
	}

	stale := time.Now().Unix()-s.UpdatedAt > 120 // >2 min without update → stale
	status := "ok"
	code := http.StatusOK
	if s.Syncing || stale {
		status = "syncing"
		code = http.StatusServiceUnavailable
	}
	if s.Peers == 0 {
		status = "no-peers"
		code = http.StatusServiceUnavailable
	}

	resp := map[string]interface{}{
		"status":     status,
		"syncing":    s.Syncing,
		"peers":      s.Peers,
		"block":      s.Block,
		"engine":     s.Engine,
		"updated_at": s.UpdatedAt,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
})
