package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type User struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	Role         string     `json:"role"`   // admin | viewer
	Status       string     `json:"status"` // active | disabled
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

const userCols = `id, username, password_hash, role, status, created_at, last_login_at`

func scanUser(row rowScanner) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, role) VALUES ($1,$2,$3) RETURNING `+userCols, username, passwordHash, role))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrUserExists
	}
	return u, err
}

func (s *Store) GetUserByName(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, username))
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) TouchLogin(ctx context.Context, id int64) {
	_, _ = s.db.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, id)
}

// UpdateUser changes role/status/password; empty values are left untouched.
func (s *Store) UpdateUser(ctx context.Context, id int64, role, status, passwordHash string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx,
		`UPDATE users SET role=COALESCE(NULLIF($2,''), role), status=COALESCE(NULLIF($3,''), status),
		   password_hash=COALESCE(NULLIF($4,''), password_hash)
		 WHERE id=$1 RETURNING `+userCols, id, role, status, passwordHash))
}

// AdminCount returns the number of active admins (to prevent locking everyone out).
func (s *Store) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	return n, err
}
