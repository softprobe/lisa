package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/softprobe/lisa/lisavm/internal/domain"
	"github.com/softprobe/lisa/lisavm/internal/manager"
)

type Server struct {
	mgr      *manager.Manager
	apiToken string
	mux      *http.ServeMux
}

func New(mgr *manager.Manager, apiToken string) *Server {
	s := &Server{
		mgr:      mgr,
		apiToken: strings.TrimSpace(apiToken),
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") && s.apiToken != "" {
		if !s.authorized(r.Header.Get("Authorization")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) authorized(header string) bool {
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	candidate := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if len(candidate) != len(s.apiToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.apiToken)) == 1
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /v1/instances", s.handleCreateInstance)
	s.mux.HandleFunc("GET /v1/instances", s.handleListInstances)
	s.mux.HandleFunc("GET /v1/instances/{id}", s.handleGetInstance)
	s.mux.HandleFunc("PATCH /v1/instances/{id}", s.handleUpdateInstance)
	s.mux.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/wake", s.handleWakeInstance)
	s.mux.HandleFunc("POST /v1/instances/{id}/sleep", s.handleSleepInstance)
	s.mux.HandleFunc("/v1/instances/{id}/proxy", s.handleProxy)
	s.mux.HandleFunc("/v1/instances/{id}/proxy/", s.handleProxy)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	inst, err := s.mgr.Create(r.Context(), req)
	if err != nil {
		writeMgrError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	ownerID := r.URL.Query().Get("owner_id")
	items := s.mgr.List(ownerID)
	if items == nil {
		items = []*domain.Instance{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.mgr.Get(r.PathValue("id"))
	if err != nil {
		writeMgrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	var req domain.UpdateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	inst, err := s.mgr.Update(r.PathValue("id"), req)
	if err != nil {
		writeMgrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeMgrError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWakeInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.mgr.EnsureRunning(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMgrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleSleepInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.mgr.Sleep(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMgrError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inst, err := s.mgr.EnsureRunning(r.Context(), id)
	if err != nil {
		writeMgrError(w, err)
		return
	}

	prefix := "/v1/instances/" + id + "/proxy"
	forwardedPath := strings.TrimPrefix(r.URL.Path, prefix)
	if forwardedPath == "" {
		forwardedPath = "/"
	}

	target, err := url.Parse("http://" + inst.IP + ":8000")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid upstream target")
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.URL.Path = forwardedPath
		req.URL.RawPath = ""
		req.Host = target.Host
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
		writeError(rw, http.StatusBadGateway, proxyErr.Error())
	}

	proxy.ServeHTTP(w, r)
	s.mgr.TouchActivity(id)
}

func writeMgrError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, manager.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, manager.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"error": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
