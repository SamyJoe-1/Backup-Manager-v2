package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"backupmanager/internal/core"
)

func (s *Server) agentList(w http.ResponseWriter, r *http.Request) (any, error) {
	return s.App.Agents()
}

func (s *Server) agentCreate(w http.ResponseWriter, r *http.Request) (any, error) {
	var in struct{ Name string }
	if err := body(r, &in); err != nil {
		return nil, err
	}
	id, tok, err := s.App.NewAgent(in.Name)
	if err != nil {
		return nil, bad("%v", err)
	}
	s.log(r, "agent.create", in.Name)
	return map[string]any{"id": id, "token": tok}, nil
}

func (s *Server) agentDelete(w http.ResponseWriter, r *http.Request) (any, error) {
	id := pathID(r)
	var n int
	s.App.Store.DB.QueryRow(`SELECT COUNT(*) FROM targets WHERE json_extract(cfg,'$.agent')=?`, id).Scan(&n)
	if n > 0 {
		return nil, bad("%d target(s) still belong to this agent; move or delete them first", n)
	}
	s.App.Store.DB.Exec(`DELETE FROM agents WHERE id=?`, id)
	s.log(r, "agent.delete", r.PathValue("id"))
	return map[string]bool{"ok": true}, nil
}

// The three calls below are made by agents, signed with the agent's own token.

func (s *Server) agentAuth(w http.ResponseWriter, r *http.Request, v any) (int64, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := s.App.AgentByToken(tok, r.Header.Get("X-BM-Host"), r.Header.Get("X-BM-Version"))
	if !ok {
		http.Error(w, `{"error":"unknown agent token"}`, http.StatusUnauthorized)
		return 0, false
	}
	if v != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(v); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return 0, false
		}
	}
	return id, true
}

func (s *Server) agentPoll(w http.ResponseWriter, r *http.Request) {
	id, ok := s.agentAuth(w, r, nil)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	t := s.App.AgentPoll(ctx, id)
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.App.Store.Audit("agent", "agent.task", t.ID, clientIP(r))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t)
}

func (s *Server) agentProgress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskID   string
		Progress json.RawMessage
	}
	id, ok := s.agentAuth(w, r, &in)
	if !ok {
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"cancel": s.App.AgentProgress(id, in.TaskID, in.Progress)})
}

func (s *Server) agentResult(w http.ResponseWriter, r *http.Request) {
	var in core.AgentResult
	id, ok := s.agentAuth(w, r, &in)
	if !ok {
		return
	}
	s.App.AgentFinish(id, in)
	w.Write([]byte("{}"))
}
