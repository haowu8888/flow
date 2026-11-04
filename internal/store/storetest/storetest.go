// Package storetest 让测试在内存与 PostgreSQL 两种 Store 上各跑一遍。
//
// PostgreSQL 用例需设置 FLOW_TEST_DATABASE_URL，每个用例使用独立 schema，结束后删除；未设置时跳过。
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"flow/internal/store"
)

func ForEach(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Run("memory", func(t *testing.T) { fn(t, store.NewMemory()) })
	t.Run("postgres", func(t *testing.T) { fn(t, Postgres(t)) })
}

func Postgres(t testing.TB) *store.Postgres {
	t.Helper()
	url := os.Getenv("FLOW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FLOW_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	rand.Read(b)
	schema := "flow_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(ctx)
	})
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return store.NewPostgres(pool)
}
