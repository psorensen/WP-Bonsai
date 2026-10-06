package sqldump

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// IsNull reports whether the field is SQL NULL.
func (f Field) IsNull() bool { return f.Type == FieldNull }

// AppendValue appends the decoded value of the field to dst. Strings lose
// their quotes, introducer, and escapes. Hex literals become their bytes.
// Numbers come back as written. NULL appends nothing. Bit literals and
// expressions return an error.
func (f Field) AppendValue(dst []byte) ([]byte, error) {
	switch f.Type {
	case FieldNull:
		return dst, nil
	case FieldNumber:
		return append(dst, f.Raw...), nil
	case FieldString:
		return appendUnquoted(dst, f.Raw)
	case FieldHex:
		return appendHex(dst, f.Raw)
	}
	return dst, fmt.Errorf("sqldump: cannot decode value %q", f.Raw)
}

// Uint64 returns the field as an unsigned integer. It accepts numbers and
// strings that hold only digits, since WordPress stores IDs in meta values as
// strings.
func (f Field) Uint64() (uint64, error) {
	switch f.Type {
	case FieldNumber:
		return strconv.ParseUint(string(f.Raw), 10, 64)
	case FieldString:
		var buf [32]byte
		v, err := appendUnquoted(buf[:0], f.Raw)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(string(v), 10, 64)
	}
	return 0, fmt.Errorf("sqldump: value %q is not an integer", f.Raw)
}

// appendUnquoted decodes a quoted string literal as MySQL reads it.
func appendUnquoted(dst, raw []byte) ([]byte, error) {
	if len(raw) > 0 && raw[0] == '_' {
		i := bytes.IndexAny(raw, `'"`)
		if i < 0 {
			return dst, fmt.Errorf("sqldump: bad string literal %q", raw)
		}
		raw = raw[i:]
	}
	if len(raw) < 2 || raw[0] != raw[len(raw)-1] || (raw[0] != '\'' && raw[0] != '"') {
		return dst, fmt.Errorf("sqldump: bad string literal %q", raw)
	}
	q := raw[0]
	s := raw[1 : len(raw)-1]
	for len(s) > 0 {
		i := bytes.IndexAny(s, string([]byte{q, '\\'}))
		if i < 0 {
			return append(dst, s...), nil
		}
		dst = append(dst, s[:i]...)
		if i+1 >= len(s) {
			return dst, errors.New("sqldump: string literal ends inside an escape")
		}
		c := s[i+1]
		if s[i] == q {
			// A doubled quote stands for one quote.
			dst = append(dst, q)
			s = s[i+2:]
			continue
		}
		switch c {
		case '0':
			dst = append(dst, 0)
		case 'b':
			dst = append(dst, '\b')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'Z':
			dst = append(dst, 0x1a)
		case '%', '_':
			// MySQL keeps the backslash so LIKE patterns still work.
			dst = append(dst, '\\', c)
		default:
			dst = append(dst, c)
		}
		s = s[i+2:]
	}
	return dst, nil
}

func appendHex(dst, raw []byte) ([]byte, error) {
	var digits []byte
	switch {
	case len(raw) >= 2 && raw[0] == '0':
		digits = raw[2:]
	case len(raw) >= 3:
		digits = raw[2 : len(raw)-1]
	default:
		return dst, fmt.Errorf("sqldump: bad hex literal %q", raw)
	}
	if len(digits)%2 == 1 {
		// MySQL pads an odd-length hex literal with a leading zero.
		digits = append([]byte{'0'}, digits...)
	}
	n := len(dst)
	dst = append(dst, make([]byte, len(digits)/2)...)
	if _, err := hex.Decode(dst[n:], digits); err != nil {
		return dst[:n], fmt.Errorf("sqldump: bad hex literal %q: %w", raw, err)
	}
	return dst, nil
}
