// key 类的 HTTP handler：put/get/delete/stat/meta/list，与 CLI key 命令三模式寻址一致
// （addr 直连 / instance 按名 / 缺省集群路由）。
package web

import (
	"io"
	"net/http"
	"strconv"
)

// maxUploadBytes key put 上传上限（整对象需内存缓冲，256MiB 够管理页运维场景）。
const maxUploadBytes = 256 << 20

func (s *Server) handleKeyStat(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errStringf("-key required"))
		return
	}
	res, err := s.svc.KeyStat(ctx, key, r.URL.Query().Get("addr"), r.URL.Query().Get("instance"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleKeyMeta(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errStringf("-key required"))
		return
	}
	res, err := s.svc.KeyMeta(ctx, key, r.URL.Query().Get("addr"), r.URL.Query().Get("instance"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	res, err := s.svc.KeyList(ctx, r.URL.Query().Get("prefix"), limit,
		r.URL.Query().Get("addr"), r.URL.Query().Get("instance"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleKeyGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errStringf("-key required"))
		return
	}
	off := int64(0)
	size := int64(-1)
	if v := r.URL.Query().Get("off"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			off = n
		}
	}
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}
	data, release, err := s.svc.KeyGet(ctx, key, off, size,
		r.URL.Query().Get("addr"), r.URL.Query().Get("instance"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	if release != nil {
		defer release()
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) handleKeyPut(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeErr(w, http.StatusBadRequest, errStringf("parse multipart: %v", err))
		return
	}
	key := firstNonEmpty(r.FormValue("key"), r.URL.Query().Get("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, errStringf("-key required"))
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errStringf("file required: %v", err))
		return
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		writeErr(w, http.StatusBadRequest, errStringf("read file: %v", err))
		return
	}
	size := int64(len(data))
	if v := r.FormValue("size"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n <= int64(len(data)) {
			size = n
		}
	}
	addr := firstNonEmpty(r.FormValue("addr"), r.URL.Query().Get("addr"))
	inst := firstNonEmpty(r.FormValue("instance"), r.URL.Query().Get("instance"))
	res, err := s.svc.KeyPut(ctx, key, data, size, addr, inst)
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.withTimeout(r)
	defer cancel()
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, errStringf("-key required"))
		return
	}
	confirm, err := parseConfirm(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !confirm {
		writeErr(w, http.StatusUnprocessableEntity, errStringf("delete 需要二次确认：confirm=true"))
		return
	}
	res, err := s.svc.KeyDelete(ctx, key, r.URL.Query().Get("addr"), r.URL.Query().Get("instance"))
	if err != nil {
		writeErr(w, apiStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
