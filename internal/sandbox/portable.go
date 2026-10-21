package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
)

// MySQL 8.0.13+ allows a default on TEXT, BLOB, JSON, and spatial columns
// only as an expression in parentheses. mariadb-dump writes these defaults
// as plain literals, which MySQL rejects with ERROR 1101. The expression form
// works on MariaDB 10.2+ as well, so the export is rewritten to use it.

// noLiteralDefaultRe matches a column definition line whose type cannot
// carry a literal default.
var noLiteralDefaultRe = regexp.MustCompile("(?i)^  `(?:[^`]|``)+` (?:tiny|medium|long)?(?:text|blob)\\b|^  `(?:[^`]|``)+` (?:json|geometry|point|linestring|polygon|multipoint|multilinestring|multipolygon|geometrycollection)\\b")

// literalDefaultRe matches a literal default: a quoted string with an
// optional character set introducer, or a number.
var literalDefaultRe = regexp.MustCompile(`^ DEFAULT ((?:_[A-Za-z0-9]+)?'(?:[^'\\]|\\.|'')*'|-?[0-9][0-9.eE+-]*)`)

// writePortable copies a mariadb-dump stream from src to dst and rewrites
// literal defaults on TEXT, BLOB, JSON, and spatial columns as expressions.
// It reads line by line and passes long lines, such as INSERTs, through in
// chunks, so memory stays flat.
func writePortable(dst io.Writer, src io.Reader) error {
	r := bufio.NewReaderSize(src, 64*1024)
	lineStart := true
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			out := chunk
			if lineStart && err == nil && noLiteralDefaultRe.Match(chunk) {
				out = wrapDefault(chunk)
			}
			if _, werr := dst.Write(out); werr != nil {
				return werr
			}
			lineStart = err == nil
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

// wrapDefault puts the literal default of a column definition line in
// parentheses. A line with no default, DEFAULT NULL, or an expression
// default is returned unchanged.
func wrapDefault(line []byte) []byte {
	i := defaultIndex(line)
	if i < 0 {
		return line
	}
	m := literalDefaultRe.FindSubmatchIndex(line[i:])
	if m == nil {
		return line
	}
	start, end := i+m[2], i+m[3]
	out := make([]byte, 0, len(line)+2)
	out = append(out, line[:start]...)
	out = append(out, '(')
	out = append(out, line[start:end]...)
	out = append(out, ')')
	return append(out, line[end:]...)
}

// defaultIndex returns the offset of " DEFAULT " outside quoted strings and
// identifiers, or -1.
func defaultIndex(line []byte) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case c == '\\' && quote == '\'':
				i++
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '`':
			quote = c
		case ' ':
			if bytes.HasPrefix(line[i:], []byte(" DEFAULT ")) {
				return i
			}
		}
	}
	return -1
}
