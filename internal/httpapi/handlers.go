package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kesha/urlshortener/internal/click"
	"github.com/kesha/urlshortener/internal/link"
)

type createLinkRequest struct {
	TargetURL  string     `json:"target_url"`
	CustomCode string     `json:"custom_code"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

type linkDTO struct {
	Code      string     `json:"code"`
	ShortURL  string     `json:"short_url"`
	TargetURL string     `json:"target_url"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Expired   bool       `json:"expired"`
	Clicks    int64      `json:"clicks"`
}

type listResponse struct {
	Items  []linkDTO `json:"items"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// createLink handles POST /api/v1/links.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	user, ok := userFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing API key")
		return
	}

	var req createLinkRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be a JSON object with target_url")
		return
	}

	l, err := s.links.Create(r.Context(), user.ID, link.CreateRequest{
		TargetURL:  req.TargetURL,
		CustomCode: req.CustomCode,
		ExpiresAt:  req.ExpiresAt,
	})
	if err != nil {
		s.writeLinkError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, s.toDTO(l))
}

// getLink handles GET /api/v1/links/{code} for the owner.
func (s *Server) getLink(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	l, err := s.links.GetForUser(r.Context(), r.PathValue("code"), user.ID)
	if err != nil {
		s.writeLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toDTO(l))
}

// listLinks handles GET /api/v1/links.
func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "offset must be zero or greater")
			return
		}
		offset = n
	}

	links, err := s.links.List(r.Context(), user.ID, limit, offset)
	if err != nil {
		s.writeLinkError(w, err)
		return
	}

	items := make([]linkDTO, 0, len(links))
	for _, l := range links {
		items = append(items, s.toDTO(l))
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items, Limit: limit, Offset: offset})
}

// deleteLink handles DELETE /api/v1/links/{code}.
func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	if err := s.links.Delete(r.Context(), r.PathValue("code"), user.ID); err != nil {
		s.writeLinkError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// redirect handles GET /{code}: the hot path.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.links.Resolve(r.Context(), code)
	if err != nil {
		if errors.Is(err, link.ErrNotFound) {
			s.notFound(w)
			return
		}
		s.log.Error("resolve failed", "code", code, "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "link resolution is temporarily unavailable")
		return
	}

	if r.Method == http.MethodGet {
		s.clicks.Record(click.Event{
			Code:        l.Code,
			Referer:     r.Referer(),
			UserAgent:   r.UserAgent(),
			VisitorHash: s.visitorHash(r),
			ClickedAt:   time.Now().UTC(),
		})
	}

	// no-cache keeps analytics honest: the browser revalidates instead of
	// serving the redirect from its own cache forever.
	w.Header().Set("Cache-Control", "no-cache")
	http.Redirect(w, r, l.TargetURL, http.StatusFound)
}

// health is liveness only: it must not touch dependencies.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readiness proves the process can serve reads, which means Postgres. Redis is
// deliberately not required: the service degrades to the database without it.
func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.ready(ctx); err != nil {
		s.log.Error("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundPage))
}

func (s *Server) toDTO(l link.Link) linkDTO {
	return linkDTO{
		Code:      l.Code,
		ShortURL:  strings.TrimRight(s.cfg.PublicBaseURL.String(), "/") + "/" + l.Code,
		TargetURL: l.TargetURL,
		CreatedAt: l.CreatedAt,
		ExpiresAt: l.ExpiresAt,
		Expired:   l.Expired(time.Now()),
		Clicks:    l.Clicks,
	}
}

func (s *Server) writeLinkError(w http.ResponseWriter, err error) {
	var ve *link.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, "invalid_request", ve.Error())
	case errors.Is(err, link.ErrCodeTaken):
		writeError(w, http.StatusConflict, "conflict", "that custom code is already taken")
	case errors.Is(err, link.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "link not found")
	default:
		s.log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

// visitorHash is a pseudonymous per-visitor identifier: daily-stable, not
// reversible, and never the raw address.
func (s *Server) visitorHash(r *http.Request) string {
	sum := sha256.Sum256([]byte(s.clientIP(r) + "|" + r.UserAgent() + "|" + s.visitorSalt))
	return hex.EncodeToString(sum[:8])
}

const notFoundPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Link not found</title>
<style>
  html,body{height:100%}
  body{margin:0;display:grid;place-items:center;background:#f6f4ef;color:#14161a;
       font-family:ui-sans-serif,"Segoe UI",system-ui,sans-serif;text-align:center;padding:24px}
  main{max-width:420px}
  p.k{font:600 12px ui-monospace,Consolas,monospace;letter-spacing:.16em;color:#8b93a1;margin:0 0 10px}
  h1{font-size:26px;letter-spacing:-.02em;margin:0 0 8px}
  p.m{color:#5d6573;font-size:14px;margin:0}
</style>
</head>
<body><main>
  <p class="k">404</p>
  <h1>This link doesn't exist</h1>
  <p class="m">It may have expired, been deleted, or never been created.</p>
</main></body>
</html>`
