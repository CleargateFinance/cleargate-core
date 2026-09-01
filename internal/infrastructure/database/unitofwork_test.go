//go:build integration

package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// The whole point of this test is to verify a later failure erases an earlier success within the same unit of work.
func TestUnitOfWork_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t) // spins Postgres via testcontainers, runs migrations

	_, err := db.Pool.Exec(ctx, `CREATE TABLE uow_probe (id int PRIMARY KEY)`)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("boom")
	err = db.UoW.Do(ctx, func(ctx context.Context) error {
		if _, err := db.Exec(ctx, `INSERT INTO uow_probe (id) VALUES (1)`); err != nil {
			return err
		}
		return sentinel // 2. Deliberately fail AFTER the insert already ran. Simulating "step 3 failed after step 1 succeeded."
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}

	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM uow_probe`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 { // rollback should happen and nothing should be written in DB
		t.Fatalf("rows = %d after rollback, want 0", n)
	}
}
