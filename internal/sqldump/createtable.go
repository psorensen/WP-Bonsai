package sqldump

import (
	"bytes"
	"fmt"
	"strings"
)

// Column is one column of a CREATE TABLE statement.
type Column struct {
	Name string
	Type string // the type as written, lower-cased, such as "bigint(20) unsigned"
}

// ParseColumns returns the columns of a CREATE TABLE statement body, in
// order. Index and constraint definitions are skipped.
func ParseColumns(body []byte) ([]Column, error) {
	open := bytes.IndexByte(body, '(')
	if open < 0 {
		return nil, fmt.Errorf("sqldump: CREATE TABLE without a column list")
	}
	defs, err := splitTopLevel(body[open+1:])
	if err != nil {
		return nil, err
	}
	var cols []Column
	for _, def := range defs {
		def = bytes.TrimSpace(def)
		if len(def) == 0 {
			continue
		}
		t := tokens{b: def}
		switch t.peekWord() {
		case "PRIMARY", "KEY", "INDEX", "UNIQUE", "CONSTRAINT", "FULLTEXT", "SPATIAL", "FOREIGN", "CHECK", "PERIOD":
			continue
		}
		name := t.ident()
		if name == "" {
			return nil, fmt.Errorf("sqldump: cannot read column definition %q", def)
		}
		t.skip()
		rest := strings.ToLower(string(def[t.i:]))
		typ := rest
		// Keep the type and its modifiers, drop NOT NULL, DEFAULT, and the rest.
		for _, stop := range []string{" not null", " null", " default", " auto_increment", " comment", " character set", " collate", " check", " generated", " as ("} {
			if i := strings.Index(typ, stop); i >= 0 {
				typ = typ[:i]
			}
		}
		cols = append(cols, Column{Name: name, Type: strings.TrimSpace(typ)})
	}
	return cols, nil
}

// splitTopLevel splits the text after the opening parenthesis of a column
// list at top-level commas and stops at the matching closing parenthesis.
func splitTopLevel(b []byte) ([][]byte, error) {
	var parts [][]byte
	depth, start := 0, 0
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case '\'', '"', '`':
			j := i + 1
			for j < len(b) {
				if b[j] == '\\' && c != '`' {
					j += 2
					continue
				}
				if b[j] == c {
					if j+1 < len(b) && b[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = j
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return append(parts, b[start:i]), nil
			}
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, b[start:i])
				start = i + 1
			}
		}
	}
	return nil, fmt.Errorf("sqldump: CREATE TABLE column list is not closed")
}
