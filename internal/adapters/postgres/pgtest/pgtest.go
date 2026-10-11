//go:build integration

// Package pgtest gives each integration test its own migrated database.
// Roles mirror production: the superuser only creates and drops databases,
// migrations run as wallet_owner and the code under test uses wallet_app, so
// tests prove that privileges, not only triggers, protect the data.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Role string

const (
	Owner Role = "wallet_owner"
	App   Role = "wallet_app"
)

type DB struct {
	Name string
	urls map[Role]string
}

// New creates a database and applies every up migration as wallet_owner.
func New(t testing.TB) *DB {
	t.Helper()
	db := NewEmpty(t)
	conn := db.Conn(t, Owner)
	if err := MigrateUp(context.Background(), conn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// NewEmpty creates a database with the same grants as the production one
// (deploy/postgres/initdb) but without running migrations.
func NewEmpty(t testing.TB) *DB {
	t.Helper()
	admin := mustEnv(t, "TEST_ADMIN_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "wallet_test_" + hex.EncodeToString(suffix)

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect as admin (is the Compose postgres up?): %v", err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE DATABASE %s OWNER wallet_owner`, name),
		fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM PUBLIC`, name),
		fmt.Sprintf(`GRANT CONNECT, TEMPORARY ON DATABASE %s TO wallet_app`, name),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
			return
		}
		defer c.Close(ctx)
		// FORCE terminates connections a failed test may have leaked.
		if _, err := c.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse TEST_ADMIN_DATABASE_URL: %v", err)
	}
	urls := map[Role]string{}
	for role, pwEnv := range map[Role]string{Owner: "WALLET_OWNER_PASSWORD", App: "WALLET_APP_PASSWORD"} {
		r := *u
		r.User = url.UserPassword(string(role), mustEnv(t, pwEnv))
		r.Path = "/" + name
		urls[role] = r.String()
	}
	return &DB{Name: name, urls: urls}
}

func (d *DB) URL(role Role) string { return d.urls[role] }

// Conn opens a single connection as role, closed at test end.
func (d *DB) Conn(t testing.TB, role Role) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, d.urls[role])
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// Pool opens a pool as role, closed at test end.
func (d *DB) Pool(t testing.TB, role Role) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), d.urls[role])
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	return p
}

// MigrationsDir is the repository's migrations directory.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "migrations")
}

// MigrateUp applies every *.up.sql in version order, as golang-migrate does,
// without adding the tool as a Go dependency.
func MigrateUp(ctx context.Context, conn *pgx.Conn) error { return apply(ctx, conn, ".up.sql", false) }

// MigrateDown applies every *.down.sql in reverse version order.
func MigrateDown(ctx context.Context, conn *pgx.Conn) error {
	return apply(ctx, conn, ".down.sql", true)
}

func apply(ctx context.Context, conn *pgx.Conn, suffix string, reverse bool) error {
	files, err := filepath.Glob(filepath.Join(MigrationsDir(), "*"+suffix))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no migration files found")
	}
	slices.Sort(files) // zero-padded version prefixes sort lexically
	if reverse {
		slices.Reverse(files)
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		// The simple protocol accepts several statements in one string, like the CLI.
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// Code returns the SQLSTATE of err, or "" if err is not a PostgreSQL error.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Constraint returns the violated constraint name of err, if any.
func Constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func mustEnv(t testing.TB, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is not set; export the variables from .env.example", key)
	}
	return v
}
