package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"kms/internal/auth"
	"kms/internal/store"
)

// ---------- login ----------

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeErr(w, 400, "username and password are required")
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	limKey := clientIP(r) + "|" + req.Username
	if s.limiter.Blocked(limKey) {
		writeErr(w, 429, "too many failed attempts; try again later")
		return
	}
	u, err := s.store.GetUserByName(r.Context(), req.Username)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (u.Status != "active" || !auth.CheckPassword(u.PasswordHash, req.Password))) {
		s.limiter.Fail(limKey)
		writeErr(w, 401, "invalid credentials")
		return
	}
	if err != nil {
		writeErr(w, 500, "login failed")
		return
	}
	s.limiter.Reset(limKey)
	tok, exp, err := s.tokens.Issue(u.ID, u.Username, u.Role)
	if err != nil {
		writeErr(w, 500, "token error")
		return
	}
	s.store.TouchLogin(r.Context(), u.ID)
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp, "user": u})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if p.Kind != "user" {
		writeJSON(w, 200, map[string]any{"username": p.Username, "role": p.Role, "kind": p.Kind})
		return
	}
	u, err := s.store.GetUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	writeJSON(w, 200, u)
}

// changePassword lets a logged-in user rotate their own password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	if p.Kind != "user" {
		writeErr(w, 400, "not a user session")
		return
	}
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.New) < 10 {
		writeErr(w, 400, "new_password must be at least 10 characters")
		return
	}
	u, err := s.store.GetUser(r.Context(), p.UserID)
	if err != nil || !auth.CheckPassword(u.PasswordHash, req.Current) {
		writeErr(w, 401, "current password is incorrect")
		return
	}
	h, _ := auth.HashPassword(req.New)
	if _, err := s.store.UpdateUser(r.Context(), u.ID, "", "", h); err != nil {
		writeErr(w, 500, "update failed")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "password changed"})
}

// ---------- user management (admin) ----------

func validRole(r string) bool { return r == auth.RoleAdmin || r == auth.RoleViewer }

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"users": us})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	if len(req.Username) < 3 || len(req.Password) < 10 {
		writeErr(w, 400, "username >= 3 chars and password >= 10 chars required")
		return
	}
	if req.Role == "" {
		req.Role = auth.RoleViewer
	}
	if !validRole(req.Role) {
		writeErr(w, 400, "role must be admin or viewer")
		return
	}
	h, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, 500, "hash failed")
		return
	}
	u, err := s.store.CreateUser(r.Context(), req.Username, h, req.Role)
	if errors.Is(err, store.ErrUserExists) {
		writeErr(w, 409, "username already exists")
		return
	}
	if err != nil {
		writeErr(w, 500, "create failed")
		return
	}
	writeJSON(w, 201, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid user id")
		return
	}
	var req struct {
		Role     string `json:"role"`
		Status   string `json:"status"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if req.Role != "" && !validRole(req.Role) {
		writeErr(w, 400, "role must be admin or viewer")
		return
	}
	if req.Status != "" && req.Status != "active" && req.Status != "disabled" {
		writeErr(w, 400, "status must be active or disabled")
		return
	}
	if req.Password != "" && len(req.Password) < 10 {
		writeErr(w, 400, "password >= 10 chars")
		return
	}
	target, err := s.store.GetUser(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "user not found")
		return
	}
	// Never remove the last active admin.
	demoting := target.Role == auth.RoleAdmin && target.Status == "active" && (req.Role == auth.RoleViewer || req.Status == "disabled")
	if demoting {
		if n, _ := s.store.AdminCount(r.Context()); n <= 1 {
			writeErr(w, 409, "cannot demote or disable the last active admin")
			return
		}
	}
	h := ""
	if req.Password != "" {
		h, _ = auth.HashPassword(req.Password)
	}
	u, err := s.store.UpdateUser(r.Context(), id, req.Role, req.Status, h)
	if err != nil {
		writeErr(w, 500, "update failed")
		return
	}
	writeJSON(w, 200, u)
}

// Bootstrap creates the first admin from env when the users table is empty.
func (s *Server) Bootstrap(username, password string) (created bool, err error) {
	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()
	n, err := s.store.CountUsers(ctx)
	if err != nil || n > 0 {
		return false, err
	}
	if password == "" {
		return false, errors.New("no users exist and ADMIN_PASSWORD is not set")
	}
	h, err := auth.HashPassword(password)
	if err != nil {
		return false, err
	}
	_, err = s.store.CreateUser(ctx, strings.ToLower(username), h, auth.RoleAdmin)
	return err == nil, err
}
