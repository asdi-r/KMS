package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"kms/internal/auth"
	"kms/internal/store"
)

// ---------- admin overview (global dashboard) ----------

func (s *Server) overviewStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context(), s.cfg.RenewalWindowDays)
	if err != nil {
		writeErr(w, 500, "stats failed")
		return
	}
	writeJSON(w, 200, st)
}

// listPurchases: all contracts (paginated, filterable). customer_id narrows to one customer.
func (s *Server) listPurchases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	f := store.ListFilter{
		Query: q.Get("q"), CustomerID: q.Get("customer_id"), Status: q.Get("status"),
		WindowDays: s.cfg.RenewalWindowDays, Limit: limit, Offset: offset,
	}
	rows, total, err := s.store.ListPurchases(r.Context(), f)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"purchases": rows, "total": total, "limit": f.Limit, "offset": f.Offset})
}

// ---------- customer portal ----------
// Login = customer_id + license key (both must match). Issues a 1h token scoped
// to that key; it can list and release that key's activations only.

func (s *Server) portalLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CustomerID string `json:"customer_id"`
		Key        string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" || req.CustomerID == "" {
		writeErr(w, 400, "customer_id and key are required")
		return
	}
	req.Key = strings.ToUpper(strings.TrimSpace(req.Key))
	limKey := clientIP(r) + "|portal|" + strings.ToLower(req.CustomerID)
	if s.limiter.Blocked(limKey) {
		writeErr(w, 429, "too many failed attempts; try again later")
		return
	}
	k, err := s.store.GetKeyForCustomer(r.Context(), req.Key, strings.TrimSpace(req.CustomerID))
	if errors.Is(err, store.ErrNotFound) {
		s.limiter.Fail(limKey)
		writeErr(w, 401, "customer_id and key do not match")
		return
	}
	if err != nil {
		writeErr(w, 500, "login failed")
		return
	}
	s.limiter.Reset(limKey)
	tok, exp, err := s.tokens.IssueCustomer(k.ID, k.CustomerID, time.Hour)
	if err != nil {
		writeErr(w, 500, "token error")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp, "customer_id": k.CustomerID, "key": k})
}

// requireCustomer gates portal routes to customer sessions.
func requireCustomer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok || p.Kind != "customer" || p.KeyID == 0 {
			writeErr(w, 403, "customer session required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) portalKey(w http.ResponseWriter, r *http.Request) (*store.Key, bool) {
	p, _ := auth.FromContext(r.Context())
	k, err := s.store.GetKeyByID(r.Context(), p.KeyID)
	if err != nil {
		writeErr(w, 404, "key not found")
		return nil, false
	}
	return k, true
}

func (s *Server) portalMe(w http.ResponseWriter, r *http.Request) {
	k, ok := s.portalKey(w, r)
	if !ok {
		return
	}
	p, err := s.store.GetPurchase(r.Context(), k.PurchaseID)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	acts, err := s.store.ListActivations(r.Context(), k.ID, r.URL.Query().Get("include") != "all")
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"key": k, "purchase": p, "activations": acts,
		"renewable": s.renewable(p.ExpiresAt), "renewable_after": p.ExpiresAt.AddDate(0, 0, -s.cfg.RenewalWindowDays),
	})
}

// portalRelease lets the customer free one of their own seats (no activation token needed).
func (s *Server) portalRelease(w http.ResponseWriter, r *http.Request) {
	k, ok := s.portalKey(w, r)
	if !ok {
		return
	}
	s.doRelease(w, r, r.Context(), k.Key, chi.URLParam(r, "device"), "")
}

func (s *Server) portalEvents(w http.ResponseWriter, r *http.Request) {
	k, ok := s.portalKey(w, r)
	if !ok {
		return
	}
	evs, err := s.store.ListEvents(r.Context(), k.PurchaseID)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

// searchCustomers powers the Customer ID autocomplete (min 1 char; UI asks from 3).
func (s *Server) searchCustomers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, map[string]any{"customers": []string{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	cs, err := s.store.SearchCustomers(r.Context(), q, limit)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"customers": cs})
}
