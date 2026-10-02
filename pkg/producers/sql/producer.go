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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" driver
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	"github.com/rs/zerolog/log"
)

// Reserved parameter names, available in statements when the record has no field with the same name.
const (
	// KeyParam is bound to the generated key.
	KeyParam = "_key"
	// ValueParam is bound to the generated value, as text.
	ValueParam = "_value"
)

// execFunc runs a statement; it is a field so that tests can run without a database.
type execFunc func(ctx context.Context, query string, args ...any) error

// Producer writes the generated records to a SQL database.
//
// The generated value must be a JSON object. In statement mode its fields are bound to the :name placeholders
// of the configured statement; in table mode every record is inserted as a row.
type Producer struct {
	cfg  resolved
	db   *sql.DB
	exec execFunc
}

// Initialize reads the JSON configuration file and connects to the database. It stops jr on failure,
// like the other producers do.
func (p *Producer) Initialize(configFile string) {
	p.InitializeWithDefaultDriver(configFile, "")
}

// InitializeWithDefaultDriver is like Initialize, with a driver to use when the configuration doesn't set one.
func (p *Producer) InitializeWithDefaultDriver(configFile string, defaultDriver string) {
	cfgBytes, err := os.ReadFile(configFile)
	if err != nil {
		log.Fatal().Err(err).Str("file", configFile).Msg("Failed to read SQL config file")
	}

	var config Config
	dec := json.NewDecoder(bytes.NewReader(cfgBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil {
		log.Fatal().Err(err).Str("file", configFile).Msg("Failed to parse SQL config file")
	}

	if err := p.InitializeFromConfig(context.Background(), config, defaultDriver); err != nil {
		log.Fatal().Err(err).Str("file", configFile).Msg("Failed to initialize SQL producer")
	}
}

// InitializeFromConfig validates the configuration, opens the connection pool and checks the connection.
func (p *Producer) InitializeFromConfig(ctx context.Context, config Config, defaultDriver string) error {
	r, err := validate(config, defaultDriver)
	if err != nil {
		return fmt.Errorf("invalid SQL configuration: %w", err)
	}

	dsn, err := expandEnv(r.Dsn)
	if err != nil {
		return fmt.Errorf("cannot resolve dsn: %w", err)
	}

	db, err := sql.Open(r.Driver, dsn)
	if err != nil {
		return fmt.Errorf("cannot open %s connection: %w", r.Driver, err)
	}
	db.SetMaxOpenConns(r.MaxOpenConns)

	pingCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("cannot connect to the %s database: %w", r.Driver, err)
	}

	p.cfg = r
	p.db = db
	p.exec = func(ctx context.Context, query string, args ...any) error {
		_, err := db.ExecContext(ctx, query, args...)
		return err
	}
	return nil
}

// Produce runs the configured statement (or inserts a row) for one generated record.
func (p *Producer) Produce(ctx context.Context, k []byte, v []byte, _ any) {
	if err := p.produce(ctx, k, v); err != nil {
		if p.cfg.OnError == OnErrorSkip {
			log.Error().Err(err).Str("driver", p.cfg.Driver).Msg("SQL statement failed, record skipped")
			return
		}
		log.Fatal().Err(err).Str("driver", p.cfg.Driver).Msg("SQL statement failed")
	}
}

func (p *Producer) produce(ctx context.Context, k []byte, v []byte) error {
	record, err := decodeRecord(v)
	if err != nil {
		return fmt.Errorf("generated value is not a JSON object (value=%q): %w", truncate(string(v), 200), err)
	}

	var query string
	var args []any
	if p.cfg.Statement != "" {
		if _, ok := record[KeyParam]; !ok {
			record[KeyParam] = string(k)
		}
		if _, ok := record[ValueParam]; !ok {
			record[ValueParam] = string(v)
		}
		query, args, err = p.cfg.stmt.render(p.cfg.dialect, record)
	} else {
		query, args, err = buildInsert(p.cfg, record, v)
	}
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.timeout)
	defer cancel()
	if err := p.exec(ctx, query, args...); err != nil {
		return fmt.Errorf("executing %q: %w", truncate(query, 200), err)
	}
	return nil
}

// Close closes the connection pool.
func (p *Producer) Close(_ context.Context) error {
	if p.db == nil {
		return nil
	}
	if err := p.db.Close(); err != nil {
		log.Warn().Err(err).Msg("Failed to close SQL connection pool")
		return err
	}
	return nil
}

// decodeRecord parses a JSON object and converts its values to something database/sql can bind:
// integers become int64, other numbers float64, nested objects and arrays their JSON text.
func decodeRecord(v []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("value is JSON null")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the JSON object")
	}

	out := make(map[string]any, len(raw)+2)
	for name, val := range raw {
		b, err := toArg(val)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		out[name] = b
	}
	return out, nil
}

func toArg(v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid number %q: %w", t.String(), err)
		}
		return f, nil
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	default: // string, bool, nil
		return t, nil
	}
}

// buildInsert builds the INSERT statement of the table mode.
func buildInsert(r resolved, record map[string]any, rawValue []byte) (string, []any, error) {
	table, err := r.dialect.quoteTable(r.Table)
	if err != nil {
		return "", nil, err
	}

	if r.JSONColumn != "" {
		col, err := r.dialect.quoteIdent(r.JSONColumn)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, col, r.dialect.placeholder(1)),
			[]any{string(rawValue)}, nil
	}

	names := make([]string, 0, len(record))
	for name := range record {
		names = append(names, name)
	}
	sort.Strings(names) // stable column order, so prepared statements are reused
	if len(names) == 0 {
		return "", nil, fmt.Errorf("generated record has no fields to insert")
	}

	cols := make([]string, len(names))
	phs := make([]string, len(names))
	args := make([]any, len(names))
	for i, name := range names {
		cols[i], err = r.dialect.quoteIdent(name)
		if err != nil {
			return "", nil, err
		}
		phs[i] = r.dialect.placeholder(i + 1)
		args[i] = record[name]
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), strings.Join(phs, ", ")), args, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
