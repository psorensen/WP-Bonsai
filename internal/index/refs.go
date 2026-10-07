package index

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/psorensen/WP-Bonsai/internal/phpser"
)

// maxRefsPerValue caps the IDs taken from one value, so one huge list cannot
// flood the index.
const maxRefsPerValue = 1000

// metaRefs returns every number in a meta value that may be a post ID: a
// plain integer, the leaf values of a serialized array, or a list of
// integers such as "1,2,3" or [1,2,3]. Pass 1 cannot tell IDs from other
// numbers. The keep-set builder later keeps only numbers that match a post.
func metaRefs(v []byte, dst []int64) []int64 {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return dst
	}
	if n, ok := parseID(v); ok {
		return append(dst, n)
	}
	if phpser.LooksSerialized(v) {
		pv, err := phpser.Unserialize(v)
		if err != nil {
			return dst
		}
		start := len(dst)
		phpser.Walk(pv, func(x phpser.Value) {
			if len(dst)-start >= maxRefsPerValue {
				return
			}
			switch x.Kind {
			case phpser.Int:
				if x.Int > 0 {
					dst = append(dst, x.Int)
				}
			case phpser.String:
				if n, ok := parseID(x.Str); ok {
					dst = append(dst, n)
				}
			}
		})
		return dst
	}
	if v[0] == '[' && v[len(v)-1] == ']' {
		v = v[1 : len(v)-1]
	}
	return appendIDList(v, dst)
}

// appendIDList reads a comma-separated list of integers, each optionally
// quoted. Anything else in the list makes it return dst unchanged.
func appendIDList(v []byte, dst []int64) []int64 {
	if bytes.IndexByte(v, ',') < 0 {
		return dst
	}
	start := len(dst)
	for part := range bytes.SplitSeq(v, []byte(",")) {
		part = bytes.Trim(bytes.TrimSpace(part), `"'`)
		n, ok := parseID(part)
		if !ok {
			return dst[:start]
		}
		if len(dst)-start < maxRefsPerValue {
			dst = append(dst, n)
		}
	}
	return dst
}

// parseID parses a positive decimal integer with no sign or spaces.
func parseID(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 18 {
		return 0, false
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n > 0
}

// contentRef is one ID found in post content.
type contentRef struct {
	id     int64
	source string // such as "block:core/image:id", "class:wp-image", "shortcode:gallery"
}

// mayHaveContentRefs is a cheap test on the raw, still escaped, SQL value.
func mayHaveContentRefs(raw []byte) bool {
	return bytes.Contains(raw, []byte("<!-- wp:")) ||
		bytes.Contains(raw, []byte("wp-image-")) ||
		bytes.Contains(raw, []byte("[gallery"))
}

// blockAttrKeys are block attributes that hold post IDs.
var blockAttrKeys = []string{"id", "ref", "ids", "mediaId"}

// contentRefs finds post IDs in post content: block attributes, wp-image-N
// classes, and gallery shortcodes. It returns each ID and source once.
func contentRefs(content []byte, dst []contentRef) []contentRef {
	start := len(dst)
	seen := func(r contentRef) bool {
		for _, d := range dst[start:] {
			if d == r {
				return true
			}
		}
		return false
	}
	add := func(id int64, source string) {
		r := contentRef{id, source}
		if len(dst)-start < maxRefsPerValue && !seen(r) {
			dst = append(dst, r)
		}
	}

	// Blocks: <!-- wp:name {"attr":...} /-->
	for rest := content; ; {
		i := bytes.Index(rest, []byte("<!-- wp:"))
		if i < 0 {
			break
		}
		rest = rest[i+len("<!-- wp:"):]
		end := bytes.Index(rest, []byte("-->"))
		if end < 0 {
			break
		}
		tag := rest[:end]
		rest = rest[end+3:]

		nameEnd := bytes.IndexAny(tag, " \t\n")
		if nameEnd < 0 {
			continue
		}
		name := string(tag[:nameEnd])
		if !bytes.ContainsRune([]byte(name), '/') {
			name = "core/" + name
		}
		attrs := bytes.TrimSpace(tag[nameEnd:])
		attrs = bytes.TrimSpace(bytes.TrimSuffix(attrs, []byte("/")))
		if len(attrs) < 2 || attrs[0] != '{' {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(attrs, &m) != nil {
			continue
		}
		for _, key := range blockAttrKeys {
			raw, ok := m[key]
			if !ok {
				continue
			}
			for _, id := range jsonIDs(raw) {
				add(id, "block:"+name+":"+key)
			}
		}
	}

	// Classic editor images: class="wp-image-123"
	for rest := content; ; {
		i := bytes.Index(rest, []byte("wp-image-"))
		if i < 0 {
			break
		}
		rest = rest[i+len("wp-image-"):]
		j := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if id, ok := parseID(rest[:j]); ok {
			add(id, "class:wp-image")
		}
	}

	// Gallery shortcodes: [gallery ids="1,2,3"]
	for rest := content; ; {
		i := bytes.Index(rest, []byte("[gallery"))
		if i < 0 {
			break
		}
		rest = rest[i+len("[gallery"):]
		end := bytes.IndexByte(rest, ']')
		if end < 0 {
			break
		}
		sc := rest[:end]
		if k := bytes.Index(sc, []byte("ids=")); k >= 0 {
			v := sc[k+4:]
			if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
				if e := bytes.IndexByte(v[1:], v[0]); e >= 0 {
					v = v[1 : 1+e]
				}
			}
			var ids []int64
			if n, ok := parseID(bytes.TrimSpace(v)); ok {
				ids = append(ids, n)
			} else {
				ids = appendIDList(v, ids)
			}
			for _, id := range ids {
				add(id, "shortcode:gallery")
			}
		}
	}
	return dst
}

// jsonIDs reads a JSON number, numeric string, or array of them.
func jsonIDs(raw json.RawMessage) []int64 {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []int64
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case float64:
			if x > 0 && x == float64(int64(x)) {
				out = append(out, int64(x))
			}
		case string:
			if n, err := strconv.ParseInt(x, 10, 64); err == nil && n > 0 {
				out = append(out, n)
			}
		case []any:
			for _, e := range x {
				if len(out) < maxRefsPerValue {
					visit(e)
				}
			}
		}
	}
	visit(v)
	return out
}
