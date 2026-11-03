package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/mondeep31/agent-comp/internal/sandbox"
)

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req CreateSandboxRequest
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	info, err := s.m.Create(r.Context(), req.toService(Owner(r.Context())))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, SandboxList{Sandboxes: s.m.List(Owner(r.Context()))})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	info, err := s.m.Get(Owner(r.Context()), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Delete(r.Context(), Owner(r.Context()), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	info, err := s.m.Pause(r.Context(), Owner(r.Context()), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	info, err := s.m.Resume(r.Context(), Owner(r.Context()), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) snapshotSandbox(w http.ResponseWriter, r *http.Request) {
	info, err := s.m.Snapshot(r.Context(), Owner(r.Context()), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, SnapshotList{Snapshots: s.m.ListSnapshots(Owner(r.Context()))})
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteSnapshot(Owner(r.Context()), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req sandbox.ExecRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.m.Exec(r.Context(), Owner(r.Context()), r.PathValue("id"), &req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	fe, err := s.m.Stat(r.Context(), Owner(r.Context()), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fe)
}

func (s *Server) listDir(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	entries, err := s.m.ListDir(r.Context(), Owner(r.Context()), r.PathValue("id"), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, DirListing{Path: p, Entries: entries})
}

// lazyBody commits a 200 octet-stream response on the first byte, so errors
// that happen before any data still get a proper status.
type lazyBody struct {
	w       http.ResponseWriter
	started bool
}

func (b *lazyBody) start() {
	if !b.started {
		b.started = true
		b.w.Header().Set("Content-Type", "application/octet-stream")
		b.w.WriteHeader(http.StatusOK)
	}
}

func (b *lazyBody) Write(p []byte) (int, error) {
	b.start()
	return b.w.Write(p)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	body := &lazyBody{w: w}
	_, err := s.m.ReadFile(r.Context(), Owner(r.Context()), r.PathValue("id"), r.URL.Query().Get("path"), body)
	switch {
	case err == nil:
		body.start()
	case !body.started:
		s.fail(w, r, err)
	default:
		// Headers are gone; abort so the client sees a truncated body, not a short file.
		s.log.Warn("read failed mid-stream", "path", r.URL.Path, "err", err)
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var mode uint64
	if m := q.Get("mode"); m != "" {
		var err error
		if mode, err = strconv.ParseUint(m, 8, 32); err != nil {
			s.fail(w, r, fmt.Errorf("%w: mode must be octal", sandbox.ErrInvalid))
			return
		}
	}
	appendTo := q.Get("append") == "true"
	fe, err := s.m.WriteFile(r.Context(), Owner(r.Context()), r.PathValue("id"), q.Get("path"), uint32(mode), appendTo, r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fe)
}
