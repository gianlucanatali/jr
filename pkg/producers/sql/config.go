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
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// DriverPgx is the PostgreSQL driver (github.com/jackc/pgx/v5 stdlib).
	DriverPgx = "pgx"
	// DriverMySQL is the MySQL/MariaDB driver (github.com/go-sql-driver/mysql).
	DriverMySQL = "mysql"

	// OnErrorFatal stops jr at the first failed statement.
	OnErrorFatal = "fatal"
	// OnErrorSkip logs a failed statement and goes on with the next record.
	OnErrorSkip = "skip"

	defaultTimeout      = 10 * time.Second
	defaultMaxOpenConns = 4
)

// Config is the configuration of the SQL producer.
//
// Exactly one of Table or Statement must be set.
type Config struct {
	// Driver is one of "pgx" (aliases: "postgres", "postgresql") or "mysql".
	// It can be left empty only when the producer is created with a default driver.
	Driver string `json:"driver"`
	// Dsn is the connection string. ${VAR} and $VAR references are replaced with
	// the value of the environment variable, so secrets don't need to be in the file.
	Dsn string `json:"dsn"`
	// Table is the target of the INSERT mode: the top level fields of the generated
	// JSON object are the columns (or, with JSONColumn, the whole object goes in a single column).
	Table string `json:"table"`
	// JSONColumn, in INSERT mode, is a (json/jsonb) column that receives the whole generated value.
	JSONColumn string `json:"jsonColumn"`
	// Statement is a SQL statement with named placeholders (:name), bound from the fields of the generated JSON object.
	Statement string `json:"statement"`
	// Timeout is the maximum duration of a single statement (Go duration, default 10s).
	Timeout string `json:"timeout"`
	// MaxOpenConns limits the connection pool size (default 4).
	MaxOpenConns int `json:"maxOpenConns"`
	// OnError is "fatal" (default, like the other producers) or "skip".
	OnError string `json:"onError"`
}

// resolved holds the validated, ready to use form of a Config.
type resolved struct {
	Config
	dialect dialect
	timeout time.Duration
	stmt    parsedStatement // set in statement mode
}

func normalizeDriver(d string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "pgx", "postgres", "postgresql":
		return DriverPgx, nil
	case "mysql":
		return DriverMySQL, nil
	case "":
		return "", fmt.Errorf("driver is required (one of %s, %s)", DriverPgx, DriverMySQL)
	default:
		return "", fmt.Errorf("unsupported driver %q (supported: %s, %s)", d, DriverPgx, DriverMySQL)
	}
}

// expandEnv replaces ${VAR} and $VAR in s, failing if a variable is not set.
func expandEnv(s string) (string, error) {
	var missing []string
	out := os.Expand(s, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// validate checks the configuration and applies defaults. defaultDriver is used when Driver is empty.
func validate(c Config, defaultDriver string) (resolved, error) {
	r := resolved{Config: c}

	if r.Driver == "" {
		r.Driver = defaultDriver
	}
	drv, err := normalizeDriver(r.Driver)
	if err != nil {
		return r, err
	}
	r.Driver = drv
	r.dialect = dialects[drv]

	if strings.TrimSpace(r.Dsn) == "" {
		return r, fmt.Errorf("dsn is required")
	}

	hasTable := strings.TrimSpace(r.Table) != ""
	hasStmt := strings.TrimSpace(r.Statement) != ""
	switch {
	case hasTable && hasStmt:
		return r, fmt.Errorf("table and statement are mutually exclusive")
	case !hasTable && !hasStmt:
		return r, fmt.Errorf("one of table or statement is required")
	}
	if r.JSONColumn != "" && !hasTable {
		return r, fmt.Errorf("jsonColumn can only be used together with table")
	}

	r.timeout = defaultTimeout
	if r.Timeout != "" {
		r.timeout, err = time.ParseDuration(r.Timeout)
		if err != nil {
			return r, fmt.Errorf("invalid timeout %q: %w", r.Timeout, err)
		}
		if r.timeout <= 0 {
			return r, fmt.Errorf("timeout must be positive, got %q", r.Timeout)
		}
	}

	if r.MaxOpenConns < 0 {
		return r, fmt.Errorf("maxOpenConns must not be negative, got %d", r.MaxOpenConns)
	}
	if r.MaxOpenConns == 0 {
		r.MaxOpenConns = defaultMaxOpenConns
	}

	switch r.OnError {
	case "":
		r.OnError = OnErrorFatal
	case OnErrorFatal, OnErrorSkip:
	default:
		return r, fmt.Errorf("invalid onError %q (supported: %s, %s)", r.OnError, OnErrorFatal, OnErrorSkip)
	}

	if hasStmt {
		r.stmt, err = parseStatementFor(r.Statement, r.dialect)
		if err != nil {
			return r, fmt.Errorf("invalid statement: %w", err)
		}
	}
	if hasTable {
		if _, err := r.dialect.quoteTable(r.Table); err != nil {
			return r, err
		}
		if r.JSONColumn != "" {
			if _, err := r.dialect.quoteIdent(r.JSONColumn); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}
