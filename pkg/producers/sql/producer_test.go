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
	"reflect"
	"strings"
	"testing"
)

func TestParseStatementRender(t *testing.T) {
	pg := dialects[DriverPgx]
	my := dialects[DriverMySQL]
	rec := map[string]any{"id": int64(7), "qty": int64(3), "name": "o'brien"}

	tests := []struct {
		name     string
		stmt     string
		d        dialect
		wantSQL  string
		wantArgs []any
	}{
		{"basic", "UPDATE t SET q = q - :qty WHERE id = :id", pg,
			"UPDATE t SET q = q - $1 WHERE id = $2", []any{int64(3), int64(7)}},
		{"repeated name reuses number", "SELECT :id, :qty, :id", pg,
			"SELECT $1, $2, $1", []any{int64(7), int64(3)}},
		{"mysql repeats args", "SELECT :id, :qty, :id", my,
			"SELECT ?, ?, ?", []any{int64(7), int64(3), int64(7)}},
		{"cast is not a parameter", "SELECT :id::text, a::int FROM t", pg,
			"SELECT $1::text, a::int FROM t", []any{int64(7)}},
		{"quoted text is untouched", "SELECT ':id', \":qty\", :name", pg,
			"SELECT ':id', \":qty\", $1", []any{"o'brien"}},
		{"escaped quote inside literal", "SELECT 'it''s :id', :id", pg,
			"SELECT 'it''s :id', $1", []any{int64(7)}},
		{"line comment", "SELECT 1 -- :nope\n, :id", pg,
			"SELECT 1 -- :nope\n, $1", []any{int64(7)}},
		{"block comment", "SELECT /* :nope */ :id", pg,
			"SELECT /* :nope */ $1", []any{int64(7)}},
		{"dollar quoting", "SELECT $$ :nope $$, :id", pg,
			"SELECT $$ :nope $$, $1", []any{int64(7)}},
		{"tagged dollar quoting", "SELECT $f$ :nope $f$, :id", pg,
			"SELECT $f$ :nope $f$, $1", []any{int64(7)}},
		{"mysql backslash escape", `SELECT 'a\' :nope', :id`, my,
			`SELECT 'a\' :nope', ?`, []any{int64(7)}},
		{"colon followed by digit", "SELECT a[1:2], :id", pg,
			"SELECT a[1:2], $1", []any{int64(7)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := parseStatementFor(tc.stmt, tc.d)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			gotSQL, gotArgs, err := ps.render(tc.d, rec)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if gotSQL != tc.wantSQL {
				t.Errorf("sql = %q, want %q", gotSQL, tc.wantSQL)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Errorf("args = %#v, want %#v", gotArgs, tc.wantArgs)
			}
		})
	}
}

func TestValuesAreNeverInterpolated(t *testing.T) {
	d := dialects[DriverPgx]
	ps, _ := parseStatementFor("SELECT :v", d)
	evil := "x'; DROP TABLE t; --"
	q, args, err := ps.render(d, map[string]any{"v": evil})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "DROP") || args[0] != evil {
		t.Fatalf("value leaked into SQL: %q %v", q, args)
	}
}

func TestParseStatementErrors(t *testing.T) {
	d := dialects[DriverPgx]
	for name, stmt := range map[string]string{
		"no params":          "UPDATE t SET a = 1",
		"unterminated quote": "SELECT 'abc :id",
		"unterminated block": "SELECT /* :id",
		"unterminated $$":    "SELECT $$ :id",
	} {
		if _, err := parseStatementFor(stmt, d); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRenderMissingField(t *testing.T) {
	d := dialects[DriverPgx]
	ps, _ := parseStatementFor("SELECT :a, :b", d)
	_, _, err := ps.render(d, map[string]any{"a": 1})
	if err == nil || !strings.Contains(err.Error(), ":b") {
		t.Fatalf("expected error naming :b, got %v", err)
	}
}

func TestDecodeRecord(t *testing.T) {
	rec, err := decodeRecord([]byte(`{"i": 9007199254740993, "f": 1.5, "s": "x", "b": true, "n": null, "o": {"a": [1,2]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"i": int64(9007199254740993), "f": 1.5, "s": "x", "b": true, "n": nil, "o": `{"a":[1,2]}`}
	if !reflect.DeepEqual(rec, want) {
		t.Fatalf("got %#v want %#v", rec, want)
	}
	for _, bad := range []string{``, `[1]`, `"x"`, `null`, `{"a":1} {"b":2}`, `{`} {
		if _, err := decodeRecord([]byte(bad)); err == nil {
			t.Errorf("decodeRecord(%q): expected error", bad)
		}
	}
}

func TestBuildInsert(t *testing.T) {
	r, err := validate(Config{Driver: "pgx", Dsn: "x", Table: `public.us"ers`}, "")
	if err != nil {
		t.Fatal(err)
	}
	q, args, err := buildInsert(r, map[string]any{"b": int64(2), "a": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `INSERT INTO "public"."us""ers" ("a", "b") VALUES ($1, $2)`; q != want {
		t.Errorf("got %q want %q", q, want)
	}
	if !reflect.DeepEqual(args, []any{"x", int64(2)}) {
		t.Errorf("args %#v", args)
	}

	r, _ = validate(Config{Driver: "mysql", Dsn: "x", Table: "t", JSONColumn: "doc"}, "")
	q, args, _ = buildInsert(r, nil, []byte(`{"a":1}`))
	if q != "INSERT INTO `t` (`doc`) VALUES (?)" || args[0] != `{"a":1}` {
		t.Errorf("json column insert: %q %v", q, args)
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Driver: "pgx", Dsn: "postgres://h/db", Statement: "UPDATE t SET a = :a"}
	tests := []struct {
		name    string
		mutate  func(*Config)
		def     string
		wantErr string
	}{
		{"valid", func(*Config) {}, "", ""},
		{"alias postgres", func(c *Config) { c.Driver = "postgres" }, "", ""},
		{"default driver", func(c *Config) { c.Driver = "" }, "pgx", ""},
		{"no driver", func(c *Config) { c.Driver = "" }, "", "driver is required"},
		{"bad driver", func(c *Config) { c.Driver = "oracle" }, "", "unsupported driver"},
		{"no dsn", func(c *Config) { c.Dsn = "" }, "", "dsn is required"},
		{"neither", func(c *Config) { c.Statement = "" }, "", "one of table or statement"},
		{"both", func(c *Config) { c.Table = "t" }, "", "mutually exclusive"},
		{"jsonColumn with statement", func(c *Config) { c.JSONColumn = "d" }, "", "jsonColumn"},
		{"bad timeout", func(c *Config) { c.Timeout = "soon" }, "", "invalid timeout"},
		{"negative timeout", func(c *Config) { c.Timeout = "-1s" }, "", "must be positive"},
		{"bad conns", func(c *Config) { c.MaxOpenConns = -1 }, "", "maxOpenConns"},
		{"bad onError", func(c *Config) { c.OnError = "ignore" }, "", "invalid onError"},
		{"statement without params", func(c *Config) { c.Statement = "DELETE FROM t" }, "", "invalid statement"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mutate(&c)
			r, err := validate(c, tc.def)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if r.OnError != OnErrorFatal || r.timeout == 0 || r.MaxOpenConns != defaultMaxOpenConns {
					t.Errorf("defaults not applied: %+v", r)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("JR_TEST_PW", "s3cret")
	got, err := expandEnv("postgres://u:${JR_TEST_PW}@h/db")
	if err != nil || got != "postgres://u:s3cret@h/db" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := expandEnv("x ${JR_TEST_DEFINITELY_UNSET}"); err == nil ||
		!strings.Contains(err.Error(), "JR_TEST_DEFINITELY_UNSET") {
		t.Fatalf("expected error naming the variable, got %v", err)
	}
}

func newTestProducer(t *testing.T, c Config) (*Producer, *[]call) {
	t.Helper()
	r, err := validate(c, "")
	if err != nil {
		t.Fatal(err)
	}
	var calls []call
	p := &Producer{cfg: r, exec: func(_ context.Context, q string, a ...any) error {
		calls = append(calls, call{q, a})
		return nil
	}}
	return p, &calls
}

type call struct {
	query string
	args  []any
}

func TestProduceStatementMode(t *testing.T) {
	p, calls := newTestProducer(t, Config{Driver: "pgx", Dsn: "x",
		Statement: "UPDATE inventory SET quantity = quantity - :qty WHERE id = :id AND tag = :_key"})
	if err := p.produce(context.Background(), []byte("k1"), []byte(`{"id": 5, "qty": 2}`)); err != nil {
		t.Fatal(err)
	}
	want := call{"UPDATE inventory SET quantity = quantity - $1 WHERE id = $2 AND tag = $3",
		[]any{int64(2), int64(5), "k1"}}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("got %#v want %#v", *calls, want)
	}
}

func TestProduceErrors(t *testing.T) {
	p, calls := newTestProducer(t, Config{Driver: "pgx", Dsn: "x", Statement: "SELECT :id"})
	if err := p.produce(context.Background(), nil, []byte(`not json`)); err == nil {
		t.Error("expected error for non JSON value")
	}
	if err := p.produce(context.Background(), nil, []byte(`{"other": 1}`)); err == nil ||
		!strings.Contains(err.Error(), ":id") {
		t.Errorf("expected missing field error, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should have been executed, got %v", *calls)
	}
}

func TestCloseWithoutConnection(t *testing.T) {
	if err := (&Producer{}).Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
