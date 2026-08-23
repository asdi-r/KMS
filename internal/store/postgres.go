// Package store is the "Aurora" box: persistent storage (Postgres-compatible).
//
// Model: a Purchase is a licence contract (customer, product, term, expires_at,
// quantity = seat quota). Each contract has ONE active license key carrying
// `seats` = quota. Endpoints activate against the key with a device_id; the
// number of active activations can never exceed seats. Renewal extends the
// contract/key; adding quantity raises seats; re-issue replaces the key string
// and carries activations over.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrSeatsFull    = errors.New("seat limit reached")
	ErrKeyInactive  = errors.New("key is not active")
	ErrKeyExpired   = errors.New("key expired")
	ErrNotActivated = errors.New("device not activated")
	ErrBadToken     = errors.New("invalid activation token")
	ErrUserExists   = errors.New("username already exists")
)

type Purchase struct {
	ID         int64     `json:"id"`
	CustomerID string    `json:"customer_id"`
	Product    string    `json:"product"`
	Quantity   int       `json:"quantity"` // seat quota (number of endpoints)
	TermYears  int       `json:"term_years"`
	TermMonths int       `json:"term_months"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Key struct {
	ID         int64     `json:"id"`
	Key        string    `json:"key"`
	PurchaseID int64     `json:"purchase_id"`
	CustomerID string    `json:"customer_id"`
	Product    string    `json:"product"`
	Status     string    `json:"status"` // active | revoked | reissued
	Seats      int       `json:"seats"`
	UsedSeats  int       `json:"used_seats"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type Activation struct {
	ID          int64      `json:"id"`
	KeyID       int64      `json:"-"`
	DeviceID    string     `json:"device_id"`
	Hostname    string     `json:"hostname,omitempty"`
	Status      string     `json:"status"` // active | deactivated
	ActivatedAt time.Time  `json:"activated_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

type Event struct {
	ID         int64     `json:"id"`
	PurchaseID int64     `json:"purchase_id"`
	Key        string    `json:"key,omitempty"`
	Action     string    `json:"action"`
	Actor      string    `json:"actor,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Store struct{ db *pgxpool.Pool }

func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{db: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

const schema = `
CREATE TABLE IF NOT EXISTS purchases (
  id          BIGSERIAL PRIMARY KEY,
  customer_id TEXT NOT NULL,
  product     TEXT NOT NULL,
  quantity    INT  NOT NULL CHECK (quantity > 0),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE purchases ADD COLUMN IF NOT EXISTS term_years INT NOT NULL DEFAULT 1;
ALTER TABLE purchases ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE purchases ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE purchases ADD COLUMN IF NOT EXISTS term_months INT;
UPDATE purchases SET term_months = term_years * 12 WHERE term_months IS NULL;
ALTER TABLE purchases ALTER COLUMN term_months SET NOT NULL;
CREATE INDEX IF NOT EXISTS purchases_customer_idx ON purchases(customer_id);

CREATE TABLE IF NOT EXISTS license_keys (
  id          BIGSERIAL PRIMARY KEY,
  key         TEXT NOT NULL UNIQUE,
  purchase_id BIGINT NOT NULL REFERENCES purchases(id),
  customer_id TEXT NOT NULL,
  product     TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'active',
  expires_at  TIMESTAMPTZ NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE license_keys ADD COLUMN IF NOT EXISTS seats INT NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS license_keys_customer_idx ON license_keys(customer_id);
CREATE INDEX IF NOT EXISTS license_keys_purchase_idx ON license_keys(purchase_id);

UPDATE purchases p SET expires_at = k.max_exp
  FROM (SELECT purchase_id, max(expires_at) AS max_exp FROM license_keys GROUP BY purchase_id) k
  WHERE p.id = k.purchase_id AND p.expires_at IS NULL;
UPDATE purchases SET expires_at = created_at + interval '1 year' WHERE expires_at IS NULL;

CREATE TABLE IF NOT EXISTS activations (
  id             BIGSERIAL PRIMARY KEY,
  key_id         BIGINT NOT NULL REFERENCES license_keys(id),
  device_id      TEXT NOT NULL,
  hostname       TEXT,
  status         TEXT NOT NULL DEFAULT 'active',
  activated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  deactivated_at TIMESTAMPTZ,
  UNIQUE (key_id, device_id)
);
CREATE INDEX IF NOT EXISTS activations_key_status_idx ON activations(key_id, status);
ALTER TABLE activations ADD COLUMN IF NOT EXISTS token_hash TEXT;

CREATE TABLE IF NOT EXISTS key_events (
  id          BIGSERIAL PRIMARY KEY,
  purchase_id BIGINT NOT NULL REFERENCES purchases(id),
  key         TEXT,
  action      TEXT NOT NULL,
  detail      TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS key_events_purchase_idx ON key_events(purchase_id);
ALTER TABLE key_events ADD COLUMN IF NOT EXISTS actor TEXT;

CREATE TABLE IF NOT EXISTS users (
  id            BIGSERIAL PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'viewer',
  status        TEXT NOT NULL DEFAULT 'active',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS deliveries (
  id           BIGSERIAL PRIMARY KEY,
  message_id   TEXT NOT NULL UNIQUE,
  key          TEXT NOT NULL,
  attempts     INT  NOT NULL,
  delivered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.Exec(ctx, schema)
	return err
}

// keyCols selects a key plus its live used-seat count.
const keyCols = `k.id, k.key, k.purchase_id, k.customer_id, k.product, k.status, k.seats,
  (SELECT count(*) FROM activations a WHERE a.key_id = k.id AND a.status = 'active')::int AS used_seats,
  k.expires_at, k.created_at`
const purchaseCols = `id, customer_id, product, quantity, term_years, term_months, expires_at, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(row rowScanner) (*Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Key, &k.PurchaseID, &k.CustomerID, &k.Product, &k.Status, &k.Seats, &k.UsedSeats, &k.ExpiresAt, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func scanPurchase(row rowScanner) (*Purchase, error) {
	var p Purchase
	err := row.Scan(&p.ID, &p.CustomerID, &p.Product, &p.Quantity, &p.TermYears, &p.TermMonths, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func insertKey(ctx context.Context, tx pgx.Tx, p *Purchase, key string, seats int) (*Key, error) {
	return scanKey(tx.QueryRow(ctx,
		`WITH k AS (INSERT INTO license_keys (key, purchase_id, customer_id, product, seats, expires_at)
		            VALUES ($1,$2,$3,$4,$5,$6) RETURNING *)
		 SELECT `+keyCols+` FROM k`, key, p.ID, p.CustomerID, p.Product, seats, p.ExpiresAt))
}

type actorKey struct{}

// WithActor records who performs the operation; logEvent stores it on the audit row.
func WithActor(ctx context.Context, actor string) context.Context { return context.WithValue(ctx, actorKey{}, actor) }

func actorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}

func logEvent(ctx context.Context, tx pgx.Tx, purchaseID int64, key, action, detail string) error {
	_, err := tx.Exec(ctx, `INSERT INTO key_events (purchase_id, key, action, detail, actor) VALUES ($1, NULLIF($2,''), $3, NULLIF($4,''), NULLIF($5,''))`,
		purchaseID, key, action, detail, actorFrom(ctx))
	return err
}

// ---- 1. Generate: new contract + one key with seat quota (atomic) ----

func (s *Store) CreatePurchase(ctx context.Context, customerID, product string, termYears, seats int, key string, expires time.Time) (*Purchase, *Key, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	p, err := scanPurchase(tx.QueryRow(ctx,
		`INSERT INTO purchases (customer_id, product, quantity, term_years, term_months, expires_at) VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING `+purchaseCols, customerID, product, seats, termYears, termYears*12, expires))
	if err != nil {
		return nil, nil, err
	}
	k, err := insertKey(ctx, tx, p, key, seats)
	if err != nil {
		return nil, nil, err
	}
	if err := logEvent(ctx, tx, p.ID, k.Key, "issued", "seats="+itoa(seats)); err != nil {
		return nil, nil, err
	}
	return p, k, tx.Commit(ctx)
}

// ---- 2. Re-issuance: retrieve stored keys / contracts ----

func (s *Store) GetPurchase(ctx context.Context, id int64) (*Purchase, error) {
	return scanPurchase(s.db.QueryRow(ctx, `SELECT `+purchaseCols+` FROM purchases WHERE id=$1`, id))
}

func (s *Store) ListPurchasesByCustomer(ctx context.Context, customerID string) ([]Purchase, error) {
	rows, err := s.db.Query(ctx, `SELECT `+purchaseCols+` FROM purchases WHERE customer_id=$1 ORDER BY id DESC`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Purchase{}
	for rows.Next() {
		p, err := scanPurchase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) ListKeysByPurchase(ctx context.Context, purchaseID int64, activeOnly bool) ([]Key, error) {
	q := `SELECT ` + keyCols + ` FROM license_keys k WHERE k.purchase_id=$1`
	if activeOnly {
		q += ` AND k.status='active'`
	}
	return s.queryKeys(ctx, q+` ORDER BY k.id`, purchaseID)
}

func (s *Store) ListKeysByCustomer(ctx context.Context, customerID string) ([]Key, error) {
	return s.queryKeys(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.customer_id=$1 ORDER BY k.id DESC`, customerID)
}

func (s *Store) queryKeys(ctx context.Context, q string, args ...any) ([]Key, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) GetKey(ctx context.Context, key string) (*Key, error) {
	return scanKey(s.db.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.key=$1`, key))
}

// ReissueKey replaces a (lost/compromised) key string with a fresh one on the
// same contract, seats and expiry. Activations are carried over so endpoints
// keep their seats; the old key stops validating.
func (s *Store) ReissueKey(ctx context.Context, oldKey, newKey string) (*Key, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	old, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.key=$1 FOR UPDATE OF k`, oldKey))
	if err != nil {
		return nil, err
	}
	if old.Status != "active" {
		return nil, ErrKeyInactive
	}
	if _, err := tx.Exec(ctx, `UPDATE license_keys SET status='reissued' WHERE id=$1`, old.ID); err != nil {
		return nil, err
	}
	var nkID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO license_keys (key, purchase_id, customer_id, product, seats, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		newKey, old.PurchaseID, old.CustomerID, old.Product, old.Seats, old.ExpiresAt).Scan(&nkID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE activations SET key_id=$2 WHERE key_id=$1`, old.ID, nkID); err != nil {
		return nil, err
	}
	nk, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.id=$1`, nkID))
	if err != nil {
		return nil, err
	}
	if err := logEvent(ctx, tx, old.PurchaseID, newKey, "reissued", "replaces "+oldKey); err != nil {
		return nil, err
	}
	return nk, tx.Commit(ctx)
}

func (s *Store) RevokeKey(ctx context.Context, key string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var pid int64
	err = tx.QueryRow(ctx, `UPDATE license_keys SET status='revoked' WHERE key=$1 AND status='active' RETURNING purchase_id`, key).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := logEvent(ctx, tx, pid, key, "revoked", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- 3. Renewal (+ optional seat increase) ----

// RenewPurchase extends the contract by termMonths, extends the active key(s),
// and optionally adds seats in the same transaction.
func (s *Store) RenewPurchase(ctx context.Context, id int64, termMonths int, newExpiry time.Time, addSeats int) (*Purchase, []Key, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	p, err := scanPurchase(tx.QueryRow(ctx,
		`UPDATE purchases SET expires_at=$2, term_months=$3, term_years=$3/12, quantity=quantity+$4, updated_at=now()
		 WHERE id=$1 RETURNING `+purchaseCols, id, newExpiry, termMonths, addSeats))
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE license_keys SET expires_at=$2, seats=seats+$3 WHERE purchase_id=$1 AND status='active'`, id, newExpiry, addSeats); err != nil {
		return nil, nil, err
	}
	keys, err := s.txKeys(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := logEvent(ctx, tx, id, "", "renewed", newExpiry.Format(time.RFC3339)); err != nil {
		return nil, nil, err
	}
	if addSeats > 0 {
		if err := logEvent(ctx, tx, id, "", "seats_added", "+"+itoa(addSeats)+" on renewal"); err != nil {
			return nil, nil, err
		}
	}
	return p, keys, tx.Commit(ctx)
}

// ---- 4. Add quantity: raise the seat quota of the contract's active key ----

func (s *Store) AddSeats(ctx context.Context, id int64, n int) (*Purchase, []Key, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	p, err := scanPurchase(tx.QueryRow(ctx,
		`UPDATE purchases SET quantity = quantity + $2, updated_at=now() WHERE id=$1 RETURNING `+purchaseCols, id, n))
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE license_keys SET seats = seats + $2 WHERE purchase_id=$1 AND status='active'`, id, n); err != nil {
		return nil, nil, err
	}
	keys, err := s.txKeys(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := logEvent(ctx, tx, id, "", "seats_added", "+"+itoa(n)); err != nil {
		return nil, nil, err
	}
	return p, keys, tx.Commit(ctx)
}

func (s *Store) txKeys(ctx context.Context, tx pgx.Tx, purchaseID int64) ([]Key, error) {
	rows, err := tx.Query(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.purchase_id=$1 AND k.status='active' ORDER BY k.id`, purchaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ---- Activation: seat enforcement ----

// Activate claims a seat for device_id. Idempotent for an already-active
// device (refreshes last_seen). Fails with ErrSeatsFull when used >= seats.
// The key row is locked so concurrent activations cannot oversubscribe.
// tokenHash is stored with the activation; the endpoint must present the matching
// token to deactivate. Re-activating an active device rotates the token.
func (s *Store) Activate(ctx context.Context, key, deviceID, hostname, tokenHash string) (*Key, *Activation, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	k, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.key=$1 FOR UPDATE OF k`, key))
	if err != nil {
		return nil, nil, err
	}
	if k.Status != "active" {
		return k, nil, ErrKeyInactive
	}
	if time.Now().After(k.ExpiresAt) {
		return k, nil, ErrKeyExpired
	}
	// Existing active activation for this device -> refresh, no new seat.
	a, err := scanActivation(tx.QueryRow(ctx,
		`UPDATE activations SET last_seen_at=now(), hostname=COALESCE(NULLIF($3,''), hostname), token_hash=$4
		 WHERE key_id=$1 AND device_id=$2 AND status='active' RETURNING `+actCols, k.ID, deviceID, hostname, tokenHash))
	if err == nil {
		return k, a, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}
	if k.UsedSeats >= k.Seats {
		return k, nil, ErrSeatsFull
	}
	a, err = scanActivation(tx.QueryRow(ctx,
		`INSERT INTO activations (key_id, device_id, hostname, token_hash) VALUES ($1,$2,NULLIF($3,''),$4)
		 ON CONFLICT (key_id, device_id) DO UPDATE
		   SET status='active', activated_at=now(), last_seen_at=now(), deactivated_at=NULL, token_hash=EXCLUDED.token_hash,
		       hostname=COALESCE(NULLIF(EXCLUDED.hostname,''), activations.hostname)
		 RETURNING `+actCols, k.ID, deviceID, hostname, tokenHash))
	if err != nil {
		return nil, nil, err
	}
	k.UsedSeats++
	if err := logEvent(ctx, tx, k.PurchaseID, k.Key, "activated", deviceID); err != nil {
		return nil, nil, err
	}
	return k, a, tx.Commit(ctx)
}

// Deactivate releases the seat held by device_id. If tokenHash is non-empty it
// must match the activation token (endpoint self-service); admins pass "".
func (s *Store) Deactivate(ctx context.Context, key, deviceID, tokenHash string) (*Key, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	k, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM license_keys k WHERE k.key=$1 FOR UPDATE OF k`, key))
	if err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx,
		`UPDATE activations SET status='deactivated', deactivated_at=now(), token_hash=NULL
		 WHERE key_id=$1 AND device_id=$2 AND status='active' AND ($3 = '' OR token_hash=$3)`, k.ID, deviceID, tokenHash)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM activations WHERE key_id=$1 AND device_id=$2 AND status='active'`, k.ID, deviceID).Scan(&n)
		if n > 0 {
			return k, ErrBadToken
		}
		return k, ErrNotActivated
	}
	k.UsedSeats--
	if err := logEvent(ctx, tx, k.PurchaseID, k.Key, "deactivated", deviceID); err != nil {
		return nil, err
	}
	return k, tx.Commit(ctx)
}

// IsDeviceActive reports whether device_id currently holds a seat on the key.
func (s *Store) IsDeviceActive(ctx context.Context, keyID int64, deviceID string) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM activations WHERE key_id=$1 AND device_id=$2 AND status='active'`, keyID, deviceID).Scan(&n)
	return n > 0, err
}

func (s *Store) TouchDevice(ctx context.Context, keyID int64, deviceID string) {
	_, _ = s.db.Exec(ctx, `UPDATE activations SET last_seen_at=now() WHERE key_id=$1 AND device_id=$2 AND status='active'`, keyID, deviceID)
}

const actCols = `id, key_id, device_id, COALESCE(hostname,''), status, activated_at, last_seen_at, deactivated_at`

func scanActivation(row rowScanner) (*Activation, error) {
	var a Activation
	err := row.Scan(&a.ID, &a.KeyID, &a.DeviceID, &a.Hostname, &a.Status, &a.ActivatedAt, &a.LastSeenAt, &a.DeactivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) ListActivations(ctx context.Context, keyID int64, activeOnly bool) ([]Activation, error) {
	q := `SELECT ` + actCols + ` FROM activations WHERE key_id=$1`
	if activeOnly {
		q += ` AND status='active'`
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY activated_at`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activation{}
	for rows.Next() {
		a, err := scanActivation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) ListEvents(ctx context.Context, purchaseID int64) ([]Event, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, purchase_id, COALESCE(key,''), action, COALESCE(actor,''), COALESCE(detail,''), created_at FROM key_events WHERE purchase_id=$1 ORDER BY id`, purchaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.PurchaseID, &e.Key, &e.Action, &e.Actor, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordDelivery is idempotent: returns false if the message was already recorded.
func (s *Store) RecordDelivery(ctx context.Context, messageID, key string, attempts int) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`INSERT INTO deliveries (message_id, key, attempts) VALUES ($1,$2,$3) ON CONFLICT (message_id) DO NOTHING`,
		messageID, key, attempts)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
