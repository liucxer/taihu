// cluster / client / instance 类的 HTTP handler：只调用 Service，不感知实现。
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// withTimeout 派生单请求上下文（Server.timeout）。
func (s *Server) withTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.timeout)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	v, err := s.svc.Version(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleClusterList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	rows, err := s.svc.ClusterList(ctx)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	rows, err := s.svc.ClusterStatus(ctx)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handleClusterIndex(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	res, err := s.svc.ClusterIndex(ctx, r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClusterPurge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	confirm, err := parseConfirm(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.ClusterPurge(ctx, confirm)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClientList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	res, err := s.svc.ClientList(ctx)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClientInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	res, err := s.svc.ClientInfo(ctx)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleInstanceSegments(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	detail := r.URL.Query().Get("detail") == "1" || r.URL.Query().Get("detail") == "true"
	res, err := s.svc.InstanceSegments(ctx, r.URL.Query().Get("instance"), detail)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAPINotFound 未知 /api 路径。
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, fmt.Errorf("api path %q not found", r.URL.Path))
}

// parseConfirm 解析二次确认：优先 query confirm=true，其次 JSON body {"confirm":true}。
func parseConfirm(r *http.Request) (bool, error) {
	if q := r.URL.Query().Get("confirm"); q != "" {
		return strconv.ParseBool(q)
	}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
		var b struct {
			Confirm bool `json:"confirm"`
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			return false, err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return false, nil
		}
		if err := json.Unmarshal(data, &b); err != nil {
			return false, err
		}
		return b.Confirm, nil
	}
	return false, nil
}
