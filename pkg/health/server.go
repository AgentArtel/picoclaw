package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/config"
)

type Server struct {
	server           *http.Server
	mux              *http.ServeMux
	mu               sync.RWMutex
	ready            bool
	checks           map[string]Check
	startTime        time.Time
	agentLoop        *agent.AgentLoop
	reloadFn         func(cfg *config.Config) error
	routesRegistered bool
}

// AdminConfigRequest is the payload for POST /v1/admin/config.
type AdminConfigRequest struct {
	Config     json.RawMessage            `json:"config"`
	Workspaces map[string]WorkspaceFiles  `json:"workspaces"`
}

// WorkspaceFiles maps filenames to content for a single agent workspace.
type WorkspaceFiles struct {
	Files map[string]string `json:"files"`
}

type Check struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type StatusResponse struct {
	Status string           `json:"status"`
	Uptime string           `json:"uptime"`
	Checks map[string]Check `json:"checks,omitempty"`
}

func NewServer(host string, port int) *Server {
	mux := http.NewServeMux()
	s := &Server{
		mux:       mux,
		ready:     false,
		checks:    make(map[string]Check),
		startTime: time.Now(),
	}

	mux.HandleFunc("/health", s.healthHandler)
	mux.HandleFunc("/ready", s.readyHandler)

	addr := fmt.Sprintf("%s:%d", host, port)
	s.server = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	return s
}

func (s *Server) Start() error {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return s.server.ListenAndServe()
}

func (s *Server) StartContext(ctx context.Context) error {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return s.server.Shutdown(context.Background())
	}
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	return s.server.Shutdown(ctx)
}

func (s *Server) SetReady(ready bool) {
	s.mu.Lock()
	s.ready = ready
	s.mu.Unlock()
}

func (s *Server) RegisterCheck(name string, checkFn func() (bool, string)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	status, msg := checkFn()
	s.checks[name] = Check{
		Name:      name,
		Status:    statusString(status),
		Message:   msg,
		Timestamp: time.Now(),
	}
}

func (s *Server) SetAgentLoop(loop *agent.AgentLoop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentLoop = loop
	if !s.routesRegistered {
		s.mux.HandleFunc("POST /v1/chat", s.chatHandler)
		s.mux.HandleFunc("POST /v1/admin/config", s.adminConfigHandler)
		s.routesRegistered = true
	}
}

func (s *Server) SetReloadFn(fn func(cfg *config.Config) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadFn = fn
}

func (s *Server) chatHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message    string `json:"message"`
		SessionKey string `json:"session_key"`
		Channel    string `json:"channel,omitempty"`
		AgentID    string `json:"agent_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	loop := s.agentLoop
	s.mu.RUnlock()
	if loop == nil {
		http.Error(w, `{"error":"agent not ready"}`, http.StatusServiceUnavailable)
		return
	}

	channel := req.Channel
	if channel == "" {
		channel = "http"
	}
	sessionKey := req.SessionKey
	if sessionKey == "" {
		sessionKey = "http:default"
	}

	var response string
	var err error
	if req.AgentID != "" {
		response, err = loop.ProcessDirectForAgent(r.Context(), req.AgentID, req.Message, sessionKey, channel, "api")
	} else {
		response, err = loop.ProcessDirectWithChannel(r.Context(), req.Message, sessionKey, channel, "api")
	}
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"response":    response,
		"session_key": sessionKey,
	})
}

func (s *Server) adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	var req AdminConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	// Parse config
	var cfg config.Config
	if err := json.Unmarshal(req.Config, &cfg); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("invalid config: %s", err.Error())})
		return
	}

	// Write workspace files for each agent
	for agentID, ws := range req.Workspaces {
		// Determine workspace path: check agents.list for agent-specific workspace,
		// fall back to defaults workspace + agent ID subdirectory
		workspaceDir := filepath.Join(cfg.Agents.Defaults.Workspace, agentID)
		for _, ac := range cfg.Agents.List {
			if ac.ID == agentID && ac.Workspace != "" {
				workspaceDir = ac.Workspace
				break
			}
		}

		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("mkdir %s: %s", workspaceDir, err.Error())})
			return
		}

		for filename, content := range ws.Files {
			filePath := filepath.Join(workspaceDir, filename)
			if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("mkdir for %s: %s", filename, err.Error())})
				return
			}
			if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("write %s: %s", filename, err.Error())})
				return
			}
		}
	}

	// Write config.json to disk
	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, ".picoclaw", "config.json")
	if err := config.SaveConfig(configPath, &cfg); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("save config: %s", err.Error())})
		return
	}

	// Trigger reload
	s.mu.RLock()
	reload := s.reloadFn
	s.mu.RUnlock()
	if reload != nil {
		if err := reload(&cfg); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("reload: %s", err.Error())})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"agents":     len(cfg.Agents.List),
		"workspaces": len(req.Workspaces),
	})
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	uptime := time.Since(s.startTime)
	resp := StatusResponse{
		Status: "ok",
		Uptime: uptime.String(),
	}

	json.NewEncoder(w).Encode(resp)
}

func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	ready := s.ready
	checks := make(map[string]Check)
	for k, v := range s.checks {
		checks[k] = v
	}
	s.mu.RUnlock()

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(StatusResponse{
			Status: "not ready",
			Checks: checks,
		})
		return
	}

	for _, check := range checks {
		if check.Status == "fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(StatusResponse{
				Status: "not ready",
				Checks: checks,
			})
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	uptime := time.Since(s.startTime)
	json.NewEncoder(w).Encode(StatusResponse{
		Status: "ready",
		Uptime: uptime.String(),
		Checks: checks,
	})
}

func statusString(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}
