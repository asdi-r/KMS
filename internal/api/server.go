// Package api contains the boxes behind the API gateway:
// Get Key, Purchase (-> Keygen + Publisher), Key Validation, activation with
// seat enforcement, the licence-lifecycle operations (re-issue, renewal, add
// quantity), and admin authentication / user management.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"kms/internal/auth"
	"kms/internal/cache"
	"kms/internal/config"
	"kms/internal/keygen"
	"kms/internal/queue"
	"kms/internal/store"
)

type Server struct {
	store   *store.Store
	cache   *cache.Cache
	queue   *queue.Queue
	cfg     config.Config
	tokens  *auth.Tokens
	limiter *auth.Limiter
}

func NewServer(s *store.Store, c *cache.Cache, q *queue.Queue, cfg config.Config) *Server {
	return &Server{store: s, cache: c, queue: q, cfg: cfg,
		tokens:  auth.NewTokens(cfg.JWTSecret, cfg.JWTTTL),
		limiter: auth.NewLimiter(cfg.LoginMaxFails, cfg.LoginLockout)}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Logger, middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	// Public
	r.Get("/health", s.health)
	r.Post("/auth/login", s.login)
	// Endpoint-facing (public, rate-limited at Kong; key is the credential)
	r.Post("/validate", s.validate)     // Key Validation (redis -> lookup)
	r.Post("/activate", s.activate)     // claim a seat -> returns activation_token
	r.Post("/deactivate", s.deactivate) // release a seat (requires activation_token)

	// Authenticated (JWT user or X-API-Key)
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(s.tokens, s.cfg.APIKey, writeErr))
		r.Use(s.actorMiddleware)

		r.Get("/auth/me", s.me)
		r.Post("/auth/password", s.changePassword)

		// Read (viewer or admin)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleViewer, writeErr))
			r.Get("/purchases", s.listPurchases)
			r.Get("/purchases/{id}", s.getPurchase)
			r.Get("/purchases/{id}/events", s.purchaseEvents)
			r.Get("/keys", s.listKeys)
			r.Get("/keys/{key}", s.getKey)
			r.Get("/keys/{key}/activations", s.listActivations)
		})
		// Write (admin only)
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleAdmin, writeErr))
			r.Post("/purchase", s.purchase)                    // 1. Generate
			r.Post("/purchases/{id}/renew", s.renewPurchase)   // 3. Renewal (+ add_quantity)
			r.Post("/purchases/{id}/keys", s.addSeats)         // 4. Add quantity (seats)
			r.Post("/keys/{key}/reissue", s.reissueKey)        // 2. Re-issuance
			r.Delete("/keys/{key}", s.revokeKey)
			r.Delete("/keys/{key}/activations/{device}", s.adminRelease) // admin seat release
			r.Get("/users", s.listUsers)
			r.Post("/users", s.createUser)
			r.Patch("/users/{id}", s.updateUser)
		})
	})
	return r
}

// actorMiddleware stamps the audit actor into the request context.
func (s *Server) actorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.FromContext(r.Context()); ok {
			r = r.WithContext(store.WithActor(r.Context(), p.Actor()))
		}
		next.ServeHTTP(w, r)
	})
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeErr(w, 503, "db unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---------- events ----------

type KeyEvent struct {
	PurchaseID int64     `json:"purchase_id"`
	CustomerID string    `json:"customer_id"`
	Product    string    `json:"product"`
	Key        string    `json:"key"`
	Seats      int       `json:"seats"`
	UsedSeats  int       `json:"used_seats"`
	ExpiresAt  time.Time `json:"expires_at"`
	DeviceID   string    `json:"device_id,omitempty"`
	Actor      string    `json:"actor,omitempty"`
}

func (s *Server) publish(r *http.Request, typ string, k *store.Key, deviceID string) {
	actor := ""
	if p, ok := auth.FromContext(r.Context()); ok {
		actor = p.Actor()
	}
	ev := KeyEvent{PurchaseID: k.PurchaseID, CustomerID: k.CustomerID, Product: k.Product, Key: k.Key,
		Seats: k.Seats, UsedSeats: k.UsedSeats, ExpiresAt: k.ExpiresAt, DeviceID: deviceID, Actor: actor}
	if _, err := s.queue.Publish(r.Context(), typ, ev); err != nil {
		slog.Error("publish failed", "type", typ, "key", k.Key, "err", err)
	}
}

func (s *Server) publishAll(r *http.Request, typ string, keys []store.Key) {
	for i := range keys {
		s.publish(r, typ, &keys[i], "")
	}
}

// ---------- 1. Generate ----------

type purchaseReq struct {
	CustomerID string `json:"customer_id"`
	Product    string `json:"product"`
	Quantity   int    `json:"quantity"`   // seat quota = number of endpoints
	TermYears  int    `json:"term_years"` // 1 or 2
}

func (s *Server) validQuantity(w http.ResponseWriter, q int) (int, bool) {
	if q <= 0 {
		q = 1
	}
	if q > s.cfg.MaxQuantity {
		writeErr(w, 400, "quantity max "+strconv.Itoa(s.cfg.MaxQuantity))
		return 0, false
	}
	return q, true
}

func (s *Server) validTerm(w http.ResponseWriter, years int) (int, bool) {
	if years == 0 {
		years = s.cfg.DefaultTermYears
	}
	if !s.cfg.TermAllowed(years) {
		writeErr(w, 400, "term_years must be one of "+joinInts(s.cfg.AllowedTermYears))
		return 0, false
	}
	return years, true
}

func (s *Server) purchase(w http.ResponseWriter, r *http.Request) {
	var req purchaseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	req.CustomerID = strings.TrimSpace(req.CustomerID)
	req.Product = strings.TrimSpace(req.Product)
	if req.CustomerID == "" || req.Product == "" {
		writeErr(w, 400, "customer_id and product are required")
		return
	}
	seats, ok := s.validQuantity(w, req.Quantity)
	if !ok {
		return
	}
	years, ok := s.validTerm(w, req.TermYears)
	if !ok {
		return
	}
	expires := time.Now().AddDate(years, 0, 0)

	p, k, err := s.store.CreatePurchase(r.Context(), req.CustomerID, req.Product, years, seats, keygen.New(), expires)
	if err != nil {
		slog.Error("create purchase", "err", err)
		writeErr(w, 500, "could not create purchase")
		return
	}
	s.publish(r, "key.issued", k, "")
	writeJSON(w, 201, map[string]any{"purchase": p, "key": k})
}

// ---------- 2. Re-issuance ----------

func (s *Server) purchaseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid purchase id")
		return 0, false
	}
	return id, true
}

func (s *Server) loadPurchase(w http.ResponseWriter, r *http.Request) (*store.Purchase, bool) {
	id, ok := s.purchaseID(w, r)
	if !ok {
		return nil, false
	}
	p, err := s.store.GetPurchase(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "purchase not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return nil, false
	}
	return p, true
}

func (s *Server) getPurchase(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPurchase(w, r)
	if !ok {
		return
	}
	activeOnly := r.URL.Query().Get("include") != "all"
	keys, err := s.store.ListKeysByPurchase(r.Context(), p.ID, activeOnly)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"purchase":        p,
		"keys":            keys,
		"renewable":       s.renewable(p.ExpiresAt),
		"renewable_after": p.ExpiresAt.AddDate(0, 0, -s.cfg.RenewalWindowDays),
	})
}

func (s *Server) listPurchases(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("customer_id")
	if cid == "" {
		writeErr(w, 400, "customer_id is required")
		return
	}
	ps, err := s.store.ListPurchasesByCustomer(r.Context(), cid)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"purchases": ps})
}

func (s *Server) purchaseEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.purchaseID(w, r)
	if !ok {
		return
	}
	evs, err := s.store.ListEvents(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"events": evs})
}

func (s *Server) loadKey(w http.ResponseWriter, r *http.Request) (*store.Key, bool) {
	k, err := s.store.GetKey(r.Context(), chi.URLParam(r, "key"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "key not found")
		return nil, false
	}
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return nil, false
	}
	return k, true
}

func (s *Server) getKey(w http.ResponseWriter, r *http.Request) {
	if k, ok := s.loadKey(w, r); ok {
		writeJSON(w, 200, k)
	}
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("customer_id")
	if cid == "" {
		writeErr(w, 400, "customer_id is required")
		return
	}
	keys, err := s.store.ListKeysByCustomer(r.Context(), cid)
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"keys": keys})
}

func (s *Server) listActivations(w http.ResponseWriter, r *http.Request) {
	k, ok := s.loadKey(w, r)
	if !ok {
		return
	}
	acts, err := s.store.ListActivations(r.Context(), k.ID, r.URL.Query().Get("include") != "all")
	if err != nil {
		writeErr(w, 500, "lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"key": k, "activations": acts})
}

func (s *Server) reissueKey(w http.ResponseWriter, r *http.Request) {
	old := chi.URLParam(r, "key")
	nk, err := s.store.ReissueKey(r.Context(), old, keygen.New())
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "key not found")
		return
	}
	if errors.Is(err, store.ErrKeyInactive) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		slog.Error("reissue", "err", err)
		writeErr(w, 500, "reissue failed")
		return
	}
	_ = s.cache.Del(r.Context(), old)
	s.publish(r, "key.reissued", nk, "")
	writeJSON(w, 201, map[string]any{"old_key": old, "key": nk})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	err := s.store.RevokeKey(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "active key not found")
		return
	}
	if err != nil {
		writeErr(w, 500, "revoke failed")
		return
	}
	_ = s.cache.Del(r.Context(), key)
	writeJSON(w, 200, map[string]string{"status": "revoked"})
}

// ---------- 3. Renewal ----------

func (s *Server) renewable(expires time.Time) bool {
	return time.Now().After(expires.AddDate(0, 0, -s.cfg.RenewalWindowDays))
}

func (s *Server) renewPurchase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TermMonths  int `json:"term_months"`
		TermYears   int `json:"term_years"`
		AddQuantity int `json:"add_quantity"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	months := req.TermMonths
	if months == 0 && req.TermYears > 0 {
		months = req.TermYears * 12
	}
	if months == 0 {
		months = s.cfg.DefaultTermYears * 12
	}
	if months < s.cfg.MinRenewMonths {
		writeErr(w, 400, "term_months must be at least "+strconv.Itoa(s.cfg.MinRenewMonths))
		return
	}
	if req.AddQuantity < 0 || req.AddQuantity > s.cfg.MaxQuantity {
		writeErr(w, 400, "add_quantity must be between 0 and "+strconv.Itoa(s.cfg.MaxQuantity))
		return
	}
	p, ok := s.loadPurchase(w, r)
	if !ok {
		return
	}
	if !s.renewable(p.ExpiresAt) {
		writeJSON(w, 409, map[string]any{
			"error":           "too early to renew",
			"expires_at":      p.ExpiresAt,
			"renewable_after": p.ExpiresAt.AddDate(0, 0, -s.cfg.RenewalWindowDays),
		})
		return
	}
	base := p.ExpiresAt
	if now := time.Now(); now.After(base) {
		base = now
	}
	newExpiry := base.AddDate(0, months, 0)

	p, keys, err := s.store.RenewPurchase(r.Context(), p.ID, months, newExpiry, req.AddQuantity)
	if err != nil {
		slog.Error("renew", "err", err)
		writeErr(w, 500, "renew failed")
		return
	}
	for _, k := range keys {
		_ = s.cache.Del(r.Context(), k.Key)
	}
	s.publishAll(r, "key.renewed", keys)
	writeJSON(w, 200, map[string]any{"purchase": p, "keys": keys, "seats_added": req.AddQuantity})
}

// ---------- 4. Add quantity (seats) ----------

func (s *Server) addSeats(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Quantity int `json:"quantity"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	n, ok := s.validQuantity(w, req.Quantity)
	if !ok {
		return
	}
	p, ok := s.loadPurchase(w, r)
	if !ok {
		return
	}
	if time.Now().After(p.ExpiresAt) {
		writeErr(w, 409, "contract expired; renew before adding quantity")
		return
	}
	p, keys, err := s.store.AddSeats(r.Context(), p.ID, n)
	if err != nil {
		slog.Error("add seats", "err", err)
		writeErr(w, 500, "could not add quantity")
		return
	}
	for _, k := range keys {
		_ = s.cache.Del(r.Context(), k.Key)
	}
	s.publishAll(r, "key.seats_added", keys)
	writeJSON(w, 200, map[string]any{"purchase": p, "keys": keys, "seats_added": n})
}

// ---------- Activation (seat enforcement) ----------

type activateReq struct {
	Key             string `json:"key"`
	DeviceID        string `json:"device_id"`
	Hostname        string `json:"hostname"`
	ActivationToken string `json:"activation_token"`
}

func seatInfo(k *store.Key) map[string]any {
	return map[string]any{
		"seats": k.Seats, "used_seats": k.UsedSeats, "remaining_seats": k.Seats - k.UsedSeats,
		"expires_at": k.ExpiresAt, "product": k.Product,
	}
}

func (s *Server) activate(w http.ResponseWriter, r *http.Request) {
	var req activateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" || strings.TrimSpace(req.DeviceID) == "" {
		writeErr(w, 400, "key and device_id are required")
		return
	}
	dev := strings.TrimSpace(req.DeviceID)
	ctx := store.WithActor(r.Context(), "endpoint:"+dev)
	token, hash := auth.NewOpaqueToken()
	k, a, err := s.store.Activate(ctx, req.Key, dev, strings.TrimSpace(req.Hostname), hash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, 404, map[string]any{"activated": false, "reason": "not_found"})
		return
	case errors.Is(err, store.ErrKeyInactive):
		writeJSON(w, 403, merge(map[string]any{"activated": false, "reason": k.Status}, seatInfo(k)))
		return
	case errors.Is(err, store.ErrKeyExpired):
		writeJSON(w, 403, merge(map[string]any{"activated": false, "reason": "expired"}, seatInfo(k)))
		return
	case errors.Is(err, store.ErrSeatsFull):
		writeJSON(w, 409, merge(map[string]any{"activated": false, "reason": "seat_limit_reached"}, seatInfo(k)))
		return
	case err != nil:
		slog.Error("activate", "err", err)
		writeErr(w, 500, "activation failed")
		return
	}
	_ = s.cache.Set(r.Context(), k.Key, k)
	s.publish(r, "key.activated", k, a.DeviceID)
	// The token is shown once; the endpoint stores it to deactivate itself later.
	writeJSON(w, 200, merge(map[string]any{"activated": true, "device_id": a.DeviceID, "activated_at": a.ActivatedAt, "activation_token": token}, seatInfo(k)))
}

func (s *Server) deactivate(w http.ResponseWriter, r *http.Request) {
	var req activateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" || strings.TrimSpace(req.DeviceID) == "" || req.ActivationToken == "" {
		writeErr(w, 400, "key, device_id and activation_token are required")
		return
	}
	dev := strings.TrimSpace(req.DeviceID)
	ctx := store.WithActor(r.Context(), "endpoint:"+dev)
	s.doRelease(w, r, ctx, req.Key, dev, auth.HashToken(req.ActivationToken))
}

// adminRelease lets an admin free a seat without the endpoint's token.
func (s *Server) adminRelease(w http.ResponseWriter, r *http.Request) {
	s.doRelease(w, r, r.Context(), chi.URLParam(r, "key"), chi.URLParam(r, "device"), "")
}

func (s *Server) doRelease(w http.ResponseWriter, r *http.Request, ctx context.Context, key, dev, tokenHash string) {
	k, err := s.store.Deactivate(ctx, key, dev, tokenHash)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, 404, map[string]any{"deactivated": false, "reason": "not_found"})
		return
	case errors.Is(err, store.ErrNotActivated):
		writeJSON(w, 404, merge(map[string]any{"deactivated": false, "reason": "device_not_activated"}, seatInfo(k)))
		return
	case errors.Is(err, store.ErrBadToken):
		writeJSON(w, 403, map[string]any{"deactivated": false, "reason": "invalid_activation_token"})
		return
	case err != nil:
		slog.Error("deactivate", "err", err)
		writeErr(w, 500, "deactivation failed")
		return
	}
	_ = s.cache.Set(r.Context(), k.Key, k)
	s.publish(r, "key.deactivated", k, dev)
	writeJSON(w, 200, merge(map[string]any{"deactivated": true, "device_id": dev}, seatInfo(k)))
}

// ---------- Key Validation ----------

type validateResp struct {
	Valid          bool       `json:"valid"`
	Reason         string     `json:"reason,omitempty"`
	Product        string     `json:"product,omitempty"`
	Seats          int        `json:"seats,omitempty"`
	UsedSeats      int        `json:"used_seats"`
	RemainingSeats int        `json:"remaining_seats"`
	DeviceActive   *bool      `json:"device_active,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Source         string     `json:"source"` // cache | db
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key      string `json:"key"`
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeErr(w, 400, "key is required")
		return
	}
	ctx := r.Context()
	var k store.Key
	hit, err := s.cache.Get(ctx, req.Key, &k)
	if err != nil {
		slog.Warn("cache get", "err", err)
		hit = false
	}
	source := "cache"
	if !hit {
		source = "db"
		kp, err := s.store.GetKey(ctx, req.Key)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, 200, validateResp{Valid: false, Reason: "not_found", Source: source})
			return
		}
		if err != nil {
			writeErr(w, 500, "lookup failed")
			return
		}
		k = *kp
		if err := s.cache.Set(ctx, req.Key, k); err != nil {
			slog.Warn("cache set", "err", err)
		}
	}
	resp := validateResp{Product: k.Product, Seats: k.Seats, UsedSeats: k.UsedSeats,
		RemainingSeats: k.Seats - k.UsedSeats, ExpiresAt: &k.ExpiresAt, Source: source}
	switch {
	case k.Status != "active":
		resp.Reason = k.Status
	case time.Now().After(k.ExpiresAt):
		resp.Reason = "expired"
	default:
		resp.Valid = true
	}
	if resp.Valid && req.DeviceID != "" {
		active, err := s.store.IsDeviceActive(ctx, k.ID, req.DeviceID)
		if err != nil {
			writeErr(w, 500, "lookup failed")
			return
		}
		resp.DeviceActive = &active
		if !active {
			resp.Valid = false
			resp.Reason = "device_not_activated"
		} else {
			s.store.TouchDevice(ctx, k.ID, req.DeviceID)
		}
	}
	writeJSON(w, 200, resp)
}

// ---------- helpers ----------

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
