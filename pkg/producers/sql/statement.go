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
	"strconv"
	"strings"
)

// dialect captures what differs between the supported databases.
type dialect struct {
	// placeholder returns the positional placeholder for the n-th (1-based) argument.
	placeholder func(n int) string
	// quote is the identifier quote character.
	quote string
	// dollarQuotes enables $tag$...$tag$ string literals in statements.
	dollarQuotes bool
	// backslashEscapes enables backslash escapes inside '...' and "..." literals.
	backslashEscapes bool
}

var dialects = map[string]dialect{
	DriverPgx: {
		placeholder:  func(n int) string { return "$" + strconv.Itoa(n) },
		quote:        `"`,
		dollarQuotes: true,
	},
	DriverMySQL: {
		placeholder:      func(int) string { return "?" },
		quote:            "`",
		backslashEscapes: true,
	},
}

func (d dialect) quoteIdent(id string) (string, error) {
	if id == "" || strings.ContainsRune(id, 0) {
		return "", fmt.Errorf("invalid identifier %q", id)
	}
	return d.quote + strings.ReplaceAll(id, d.quote, d.quote+d.quote) + d.quote, nil
}

// quoteTable quotes a possibly schema-qualified ("schema.table") name.
func (d dialect) quoteTable(name string) (string, error) {
	parts := strings.Split(strings.TrimSpace(name), ".")
	for i, p := range parts {
		q, err := d.quoteIdent(strings.TrimSpace(p))
		if err != nil {
			return "", fmt.Errorf("invalid table name %q: %w", name, err)
		}
		parts[i] = q
	}
	return strings.Join(parts, "."), nil
}

// segment is a piece of a parsed statement: literal SQL text, or a named parameter.
type segment struct {
	text  string
	param string // non-empty for a parameter
}

// parsedStatement is a statement split in literal text and named parameters.
type parsedStatement struct {
	segments []segment
}

// Params returns the parameter names in order of appearance (with repetitions).
func (p parsedStatement) Params() []string {
	var out []string
	for _, s := range p.segments {
		if s.param != "" {
			out = append(out, s.param)
		}
	}
	return out
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// parseStatement finds the :name placeholders of a statement. It ignores "::" casts, quoted strings and
// identifiers, dollar-quoted strings, and comments. Values are never interpolated into the SQL.
func parseStatement(stmt string) (parsedStatement, error) {
	return parseStatementFor(stmt, dialects[DriverPgx])
}

func parseStatementFor(stmt string, d dialect) (parsedStatement, error) {
	var segs []segment
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, segment{text: lit.String()})
			lit.Reset()
		}
	}

	n := len(stmt)
	for i := 0; i < n; {
		c := stmt[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := skipQuoted(stmt, i, d)
			if j < 0 {
				return parsedStatement{}, fmt.Errorf("unterminated quoted text starting at offset %d", i)
			}
			lit.WriteString(stmt[i:j])
			i = j
		case c == '-' && i+1 < n && stmt[i+1] == '-':
			j := strings.IndexByte(stmt[i:], '\n')
			if j < 0 {
				j = n - i
			}
			lit.WriteString(stmt[i : i+j])
			i += j
		case c == '/' && i+1 < n && stmt[i+1] == '*':
			j := strings.Index(stmt[i+2:], "*/")
			if j < 0 {
				return parsedStatement{}, fmt.Errorf("unterminated comment starting at offset %d", i)
			}
			end := i + 2 + j + 2
			lit.WriteString(stmt[i:end])
			i = end
		case c == '$' && d.dollarQuotes:
			if tag, ok := dollarTag(stmt, i); ok {
				j := strings.Index(stmt[i+len(tag):], tag)
				if j < 0 {
					return parsedStatement{}, fmt.Errorf("unterminated dollar-quoted text starting at offset %d", i)
				}
				end := i + len(tag) + j + len(tag)
				lit.WriteString(stmt[i:end])
				i = end
				continue
			}
			lit.WriteByte(c)
			i++
		case c == ':':
			if i+1 < n && stmt[i+1] == ':' { // cast operator
				lit.WriteString("::")
				i += 2
				continue
			}
			if i+1 < n && isNameStart(stmt[i+1]) {
				j := i + 1
				for j < n && isNameChar(stmt[j]) {
					j++
				}
				flush()
				segs = append(segs, segment{param: stmt[i+1 : j]})
				i = j
				continue
			}
			lit.WriteByte(c)
			i++
		default:
			lit.WriteByte(c)
			i++
		}
	}
	flush()

	ps := parsedStatement{segments: segs}
	if len(ps.Params()) == 0 {
		return ps, fmt.Errorf("statement has no :name placeholders, nothing would be bound from the generated record")
	}
	return ps, nil
}

// skipQuoted returns the offset right after the quoted text starting at i, or -1 if it is not terminated.
// A doubled quote character is an escaped quote.
func skipQuoted(s string, i int, d dialect) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && d.backslashEscapes && q != '`':
			j++
		case s[j] == q:
			if j+1 < len(s) && s[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return -1
}

// dollarTag returns the "$tag$" opening at i, if there is one.
func dollarTag(s string, i int) (string, bool) {
	j := i + 1
	for j < len(s) && (isNameChar(s[j])) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1], true
	}
	return "", false
}

// render builds the final SQL with positional placeholders and the matching argument list.
// The values come from record; a missing parameter is an error.
func (p parsedStatement) render(d dialect, record map[string]any) (string, []any, error) {
	var sb strings.Builder
	var args []any
	// For numbered placeholders a name used several times is bound once.
	index := map[string]int{}
	numbered := d.placeholder(1) != d.placeholder(2)

	for _, s := range p.segments {
		if s.param == "" {
			sb.WriteString(s.text)
			continue
		}
		v, ok := record[s.param]
		if !ok {
			return "", nil, fmt.Errorf("statement parameter :%s has no matching field in the generated record", s.param)
		}
		if numbered {
			if n, seen := index[s.param]; seen {
				sb.WriteString(d.placeholder(n))
				continue
			}
		}
		args = append(args, v)
		if numbered {
			index[s.param] = len(args)
		}
		sb.WriteString(d.placeholder(len(args)))
	}
	return sb.String(), args, nil
}
