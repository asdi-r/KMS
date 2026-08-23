package store

import (
	"context"
	"strings"
	"time"
)

// PurchaseRow is a contract with its active key summary, for list/overview views.
type PurchaseRow struct {
	Purchase
	Key       string `json:"key,omitempty"`
	KeyStatus string `json:"key_status,omitempty"`
	Seats     int    `json:"seats"`
	UsedSeats int    `json:"used_seats"`
}

type ListFilter struct {
	Query      string // matches customer_id or product (ILIKE)
	CustomerID string // exact
	Status     string // "" | active | expiring | expired
	WindowDays int    // expiring = expires within this many days
	Limit      int
	Offset     int
}

const rowCols = `p.id, p.customer_id, p.product, p.quantity, p.term_years, p.term_months, p.expires_at, p.created_at, p.updated_at,
  COALESCE(k.key,''), COALESCE(k.status,''), COALESCE(k.seats,0),
  COALESCE((SELECT count(*) FROM activations a WHERE a.key_id = k.id AND a.status='active'),0)::int`

func (f ListFilter) where(args *[]any) string {
	conds := []string{"1=1"}
	add := func(v any) string { *args = append(*args, v); return "$" + itoa(len(*args)) }
	if f.CustomerID != "" {
		conds = append(conds, "p.customer_id = "+add(f.CustomerID))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := add("%" + q + "%")
		conds = append(conds, "(p.customer_id ILIKE "+p+" OR p.product ILIKE "+p+" OR k.key ILIKE "+p+")")
	}
	switch f.Status {
	case "active":
		conds = append(conds, "p.expires_at > now() + ("+add(f.WindowDays)+" * interval '1 day')")
	case "expiring":
		conds = append(conds, "p.expires_at > now() AND p.expires_at <= now() + ("+add(f.WindowDays)+" * interval '1 day')")
	case "expired":
		conds = append(conds, "p.expires_at <= now()")
	}
	return strings.Join(conds, " AND ")
}

// ListPurchases returns contracts (newest first) matching the filter plus the total count.
func (s *Store) ListPurchases(ctx context.Context, f ListFilter) ([]PurchaseRow, int, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	args := []any{}
	where := f.where(&args)
	from := ` FROM purchases p LEFT JOIN license_keys k ON k.purchase_id = p.id AND k.status = 'active' WHERE ` + where

	var total int
	if err := s.db.QueryRow(ctx, `SELECT count(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.db.Query(ctx, `SELECT `+rowCols+from+` ORDER BY p.id DESC LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PurchaseRow{}
	for rows.Next() {
		var r PurchaseRow
		if err := rows.Scan(&r.ID, &r.CustomerID, &r.Product, &r.Quantity, &r.TermYears, &r.TermMonths, &r.ExpiresAt, &r.CreatedAt, &r.UpdatedAt,
			&r.Key, &r.KeyStatus, &r.Seats, &r.UsedSeats); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

type Stats struct {
	Contracts        int       `json:"contracts"`
	Customers        int       `json:"customers"`
	Seats            int       `json:"seats"`
	UsedSeats        int       `json:"used_seats"`
	ExpiringSoon     int       `json:"expiring_soon"`
	Expired          int       `json:"expired"`
	ActivationsToday int       `json:"activations_today"`
	RevokedKeys      int       `json:"revoked_keys"`
	WindowDays       int       `json:"window_days"`
	GeneratedAt      time.Time `json:"generated_at"`
}

func (s *Store) Stats(ctx context.Context, windowDays int) (*Stats, error) {
	st := Stats{WindowDays: windowDays, GeneratedAt: time.Now()}
	err := s.db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM purchases),
		  (SELECT count(DISTINCT customer_id) FROM purchases),
		  COALESCE((SELECT sum(seats) FROM license_keys WHERE status='active'),0),
		  (SELECT count(*) FROM activations a JOIN license_keys k ON k.id=a.key_id WHERE a.status='active' AND k.status='active'),
		  (SELECT count(*) FROM purchases WHERE expires_at > now() AND expires_at <= now() + ($1 * interval '1 day')),
		  (SELECT count(*) FROM purchases WHERE expires_at <= now()),
		  (SELECT count(*) FROM activations WHERE activated_at >= date_trunc('day', now())),
		  (SELECT count(*) FROM license_keys WHERE status='revoked')`, windowDays).
		Scan(&st.Contracts, &st.Customers, &st.Seats, &st.UsedSeats, &st.ExpiringSoon, &st.Expired, &st.ActivationsToday, &st.RevokedKeys)
	return &st, err
}

// GetKeyForCustomer is the customer-portal login check: key must belong to customer.
func (s *Store) GetKeyForCustomer(ctx context.Context, key, customerID string) (*Key, error) {
	return scanKey(s.db.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.key=$1 AND lower(k.customer_id)=lower($2)`, key, customerID))
}

func (s *Store) GetKeyByID(ctx context.Context, id int64) (*Key, error) {
	return scanKey(s.db.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.id=$1`, id))
}

// SearchCustomers returns distinct customer IDs matching the prefix/substring (for autocomplete).
func (s *Store) SearchCustomers(ctx context.Context, q string, limit int) ([]string, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.Query(ctx,
		`SELECT customer_id FROM purchases WHERE customer_id ILIKE $1 GROUP BY customer_id ORDER BY max(created_at) DESC LIMIT $2`,
		"%"+strings.TrimSpace(q)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
