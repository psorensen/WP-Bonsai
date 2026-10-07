// Package phpser reads PHP serialized values, as WordPress stores arrays in
// meta and option values. It reads only; it never builds PHP objects.
package phpser

import (
	"bytes"
	"errors"
	"strconv"
)

// Kind is the PHP type of a Value.
type Kind uint8

const (
	Null Kind = iota + 1
	Bool
	Int
	Float
	String
	Array // also used for objects, whose properties become entries
)

// Value is one decoded PHP value.
type Value struct {
	Kind    Kind
	Int     int64   // Int, and Bool as 0 or 1
	Float   float64 // Float
	Str     []byte  // String, pointing into the input
	Entries []Entry // Array
}

// Entry is one key and value of a PHP array.
type Entry struct {
	Key, Val Value
}

// maxDepth stops runaway nesting in malformed input.
const maxDepth = 64

var errBad = errors.New("phpser: not a valid serialized value")

// LooksSerialized reports whether b may be a serialized array or object.
// It is a cheap test to run before Unserialize.
func LooksSerialized(b []byte) bool {
	return len(b) >= 4 && (b[0] == 'a' || b[0] == 'O') && b[1] == ':' && b[len(b)-1] == '}'
}

// Unserialize decodes one serialized value that fills all of b.
func Unserialize(b []byte) (Value, error) {
	d := decoder{b: b}
	v, err := d.value(0)
	if err != nil {
		return Value{}, err
	}
	if d.i != len(b) {
		return Value{}, errBad
	}
	return v, nil
}

type decoder struct {
	b []byte
	i int
}

func (d *decoder) value(depth int) (Value, error) {
	if depth > maxDepth || d.i+1 >= len(d.b) {
		return Value{}, errBad
	}
	t := d.b[d.i]
	if t == 'N' {
		if d.b[d.i+1] != ';' {
			return Value{}, errBad
		}
		d.i += 2
		return Value{Kind: Null}, nil
	}
	if d.b[d.i+1] != ':' {
		return Value{}, errBad
	}
	d.i += 2
	switch t {
	case 'b', 'i':
		n, err := d.intUntil(';')
		if err != nil {
			return Value{}, err
		}
		if t == 'b' {
			return Value{Kind: Bool, Int: n}, nil
		}
		return Value{Kind: Int, Int: n}, nil
	case 'd':
		end := bytes.IndexByte(d.b[d.i:], ';')
		if end < 0 {
			return Value{}, errBad
		}
		f, err := strconv.ParseFloat(string(d.b[d.i:d.i+end]), 64)
		if err != nil {
			return Value{}, errBad
		}
		d.i += end + 1
		return Value{Kind: Float, Float: f}, nil
	case 's':
		s, err := d.str()
		if err != nil {
			return Value{}, err
		}
		if !d.expect(';') {
			return Value{}, errBad
		}
		return Value{Kind: String, Str: s}, nil
	case 'a':
		return d.array(depth)
	case 'O':
		// O:<len>:"<class>":<count>:{...}
		if _, err := d.str(); err != nil {
			return Value{}, err
		}
		if !d.expect(':') {
			return Value{}, errBad
		}
		return d.array(depth)
	}
	return Value{}, errBad
}

// array reads "<count>:{<key><value>...}".
func (d *decoder) array(depth int) (Value, error) {
	n, err := d.intUntil(':')
	if err != nil || n < 0 || n > int64(len(d.b)) {
		return Value{}, errBad
	}
	if !d.expect('{') {
		return Value{}, errBad
	}
	v := Value{Kind: Array, Entries: make([]Entry, 0, n)}
	for range n {
		k, err := d.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		val, err := d.value(depth + 1)
		if err != nil {
			return Value{}, err
		}
		v.Entries = append(v.Entries, Entry{Key: k, Val: val})
	}
	if !d.expect('}') {
		return Value{}, errBad
	}
	return v, nil
}

// str reads `<len>:"<bytes>"`. The length counts bytes, not characters.
func (d *decoder) str() ([]byte, error) {
	n, err := d.intUntil(':')
	if err != nil || n < 0 || d.i+int(n)+2 > len(d.b) {
		return nil, errBad
	}
	if d.b[d.i] != '"' || d.b[d.i+1+int(n)] != '"' {
		return nil, errBad
	}
	s := d.b[d.i+1 : d.i+1+int(n)]
	d.i += int(n) + 2
	return s, nil
}

func (d *decoder) intUntil(stop byte) (int64, error) {
	end := bytes.IndexByte(d.b[d.i:], stop)
	if end <= 0 {
		return 0, errBad
	}
	n, err := strconv.ParseInt(string(d.b[d.i:d.i+end]), 10, 64)
	if err != nil {
		return 0, errBad
	}
	d.i += end + 1
	return n, nil
}

func (d *decoder) expect(c byte) bool {
	if d.i < len(d.b) && d.b[d.i] == c {
		d.i++
		return true
	}
	return false
}

// Walk calls fn for every non-array value inside v, including v itself when
// it is not an array. Array keys are not visited.
func Walk(v Value, fn func(Value)) {
	if v.Kind != Array {
		fn(v)
		return
	}
	for _, e := range v.Entries {
		Walk(e.Val, fn)
	}
}

// Lookup returns the value stored under a string key of an array.
func (v Value) Lookup(key string) (Value, bool) {
	for _, e := range v.Entries {
		if e.Key.Kind == String && string(e.Key.Str) == key {
			return e.Val, true
		}
	}
	return Value{}, false
}
