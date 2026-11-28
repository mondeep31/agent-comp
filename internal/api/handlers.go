package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	info, err := s.m.Create(r.Context(), req.ToService(Owner(r.Context())))
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

func (s *Server) listImages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ImageList{Images: s.m.Images()})
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

// screenshot serves the sandbox's screen as an image. Query: format (png or
// jpeg), quality (1-100) and region (x0,y0,x1,y1).
func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := sandbox.ScreenshotOptions{Format: q.Get("format")}
	if v := q.Get("quality"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.fail(w, r, fmt.Errorf("%w: quality must be a number", sandbox.ErrInvalid))
			return
		}
		opts.Quality = n
	}
	if v := q.Get("region"); v != "" {
		parts := strings.Split(v, ",")
		opts.Region = make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				s.fail(w, r, fmt.Errorf("%w: region must be x0,y0,x1,y1", sandbox.ErrInvalid))
				return
			}
			opts.Region = append(opts.Region, n)
		}
	}
	img, err := s.m.Screenshot(r.Context(), Owner(r.Context()), r.PathValue("id"), opts)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", img.MIMEType)
	h.Set("Content-Length", strconv.Itoa(len(img.Data)))
	h.Set("Cache-Control", "no-store")
	// The bytes come from the guest, which is untrusted: browsers must treat
	// them as the image type the server chose, never as a document.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Screen-Width", strconv.Itoa(img.Width))
	h.Set("X-Screen-Height", strconv.Itoa(img.Height))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img.Data) //nolint:gosec // served as image/png or image/jpeg with nosniff and a sandbox CSP
}

// computer runs one computer-use action. Any screenshot comes back base64
// encoded in image.data.
func (s *Server) computer(w http.ResponseWriter, r *http.Request) {
	var req sandbox.ComputerAction
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.m.Computer(r.Context(), Owner(r.Context()), r.PathValue("id"), &req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
