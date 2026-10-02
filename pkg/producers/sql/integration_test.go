// Copyright © 2024 JR team
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// TestPostgresIntegration runs against a real PostgreSQL server. It is skipped unless
// JR_TEST_POSTGRES_DSN is set, e.g. postgres://postgres:pw@localhost:5432/postgres?sslmode=disable
func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("JR_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("JR_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS jr_sql_test; CREATE TABLE jr_sql_test (id int PRIMARY KEY, quantity int, note text, doc jsonb)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `DROP TABLE jr_sql_test`) }()

	insert := &Producer{}
	if err := insert.InitializeFromConfig(ctx, Config{Dsn: dsn, Table: "jr_sql_test"}, DriverPgx); err != nil {
		t.Fatal(err)
	}
	insert.Produce(ctx, nil, []byte(`{"id": 1, "quantity": 10, "note": "a", "doc": {"x": [1, 2]}}`), nil)
	if err := insert.Close(ctx); err != nil {
		t.Fatal(err)
	}

	update := &Producer{}
	if err := update.InitializeFromConfig(ctx, Config{Dsn: dsn,
		Statement: "UPDATE jr_sql_test SET quantity = quantity - :qty WHERE id = :id"}, DriverPgx); err != nil {
		t.Fatal(err)
	}
	update.Produce(ctx, nil, []byte(`{"id": 1, "qty": 3}`), nil)
	if err := update.Close(ctx); err != nil {
		t.Fatal(err)
	}

	var qty int
	var doc string
	if err := db.QueryRowContext(ctx, `SELECT quantity, doc::text FROM jr_sql_test WHERE id = 1`).Scan(&qty, &doc); err != nil {
		t.Fatal(err)
	}
	if qty != 7 || doc != `{"x": [1, 2]}` {
		t.Fatalf("quantity=%d doc=%s", qty, doc)
	}
}
