// Package index builds and reads the pass 1 index of a dump. The index is
// a DuckDB file that holds IDs, types, sizes, and references, but no post
// content and no personal data. See SPEC.md, "Pass 1: index".
package index

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duckdb/duckdb-go/v2"

	"github.com/psorensen/WP-Bonsai/internal/phpser"
	"github.com/psorensen/WP-Bonsai/internal/sqldump"
)

// FileName is the name of the index file inside a work directory.
const FileName = "index.duckdb"

// memoryLimit caps the memory DuckDB uses while building the index.
const memoryLimit = "1GB"

// Source describes the dump an index was built from.
type Source struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// Options controls Build.
type Options struct {
	// Progress, if set, is called about every 64 MB with the number of
	// dump bytes read so far.
	Progress func(read int64)
}

// Summary describes a finished index.
type Summary struct {
	Prefix    string
	Multisite bool
	Tables    int
	Rows      int64
	Bytes     int64
	Duration  time.Duration
	Warnings  []string
}

// roleColumns lists, per core table, the columns pass 1 reads. Row handlers
// refer to them by position in these lists.
var roleColumns = map[string][]string{
	"posts":              {"ID", "post_author", "post_date", "post_status", "post_parent", "post_type", "post_mime_type", "post_name", "post_excerpt", "post_content"},
	"postmeta":           {"meta_id", "post_id", "meta_key", "meta_value"},
	"terms":              {"term_id", "name", "slug"},
	"term_taxonomy":      {"term_taxonomy_id", "term_id", "taxonomy", "parent", "count"},
	"term_relationships": {"object_id", "term_taxonomy_id"},
	"termmeta":           {"meta_id", "term_id", "meta_key"},
	"users":              {"ID"},
	"usermeta":           {"umeta_id", "user_id", "meta_key", "meta_value"},
	"comments":           {"comment_ID", "comment_post_ID", "comment_approved", "comment_type", "comment_parent", "user_id", "comment_date"},
	"commentmeta":        {"meta_id", "comment_id", "meta_key"},
	"options":            {"option_id", "option_name", "autoload", "option_value"},
}

// Maximum meta value size that pass 1 decodes to look for IDs.
const maxRefValueBytes = 1 << 20

type tableState struct {
	name       string
	prefix     string // the part before a core suffix, when the name ends with one
	suffix     string // the core suffix, or ""
	ord        int
	cols       []sqldump.Column
	createSQL  string
	rows       int64
	rowBytes   int64
	otherBytes int64
	badRows    int64
}

type indexer struct {
	ctx  context.Context
	app  map[string]*duckdb.Appender
	tabs map[string]*tableState
	ord  int

	// The INSERT being read.
	cur       *tableState
	pos       []int // positions of roleColumns[cur.suffix] in the row
	objectPos int   // position of post_id or object_id in a non-core table, or -1
	objectCol string
	ncols     int

	fields  []sqldump.Field
	scratch []byte
	refs    []int64
	crefs   []contentRef

	warnings []string
}

// Build reads a dump from r and writes a new index to dbPath. An existing file
// at dbPath is replaced.
func Build(ctx context.Context, r io.Reader, dbPath string, src Source, opts Options) (*Summary, error) {
	start := time.Now()
	for _, p := range []string{dbPath, dbPath + ".wal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// Cap DuckDB's memory so pass 1 stays flat on any dump size. DuckDB
	// writes to disk when it needs more.
	connector, err := duckdb.NewConnector(dbPath+"?memory_limit="+memoryLimit, nil)
	if err != nil {
		return nil, err
	}
	defer connector.Close()
	db := sql.OpenDB(connector)
	defer db.Close()
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("index: create schema: %w", err)
	}

	ix := &indexer{ctx: ctx, app: map[string]*duckdb.Appender{}, tabs: map[string]*tableState{}}
	read, err := ix.stream(ctx, connector, r, opts)
	if err != nil {
		return nil, err
	}

	sum := &Summary{Bytes: read}
	if err := ix.finish(ctx, db, src, sum); err != nil {
		return nil, err
	}
	sum.Duration = time.Since(start)
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO info VALUES ('duration_seconds', ?)`,
		strconv.FormatFloat(sum.Duration.Seconds(), 'f', 1, 64)); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "CHECKPOINT"); err != nil {
		return nil, err
	}
	return sum, nil
}

// stream runs pass 1 over the dump and appends rows to the index.
func (ix *indexer) stream(ctx context.Context, connector *duckdb.Connector, r io.Reader, opts Options) (int64, error) {
	conn, err := connector.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	for _, t := range rowTables {
		a, err := duckdb.NewAppenderFromConn(conn, "", t.table)
		if err != nil {
			return 0, fmt.Errorf("index: appender for %s: %w", t.table, err)
		}
		ix.app[t.table] = a
	}
	{
		a, err := duckdb.NewAppenderFromConn(conn, "", "object_rows")
		if err != nil {
			return 0, err
		}
		ix.app["object_rows"] = a
	}

	p := sqldump.NewParser(r)
	var nextProgress int64 = 64 << 20
	for {
		it, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p.Offset(), err
		}
		if err := ix.item(it); err != nil {
			return p.Offset(), err
		}
		if opts.Progress != nil && p.Offset() >= nextProgress {
			opts.Progress(p.Offset())
			nextProgress += 64 << 20
			if err := ctx.Err(); err != nil {
				return p.Offset(), err
			}
		}
	}
	if opts.Progress != nil {
		opts.Progress(p.Offset())
	}
	for name, a := range ix.app {
		if err := a.Close(); err != nil {
			return p.Offset(), fmt.Errorf("index: flush %s: %w", name, err)
		}
	}
	return p.Offset(), nil
}

func (ix *indexer) table(name string) *tableState {
	if t, ok := ix.tabs[name]; ok {
		return t
	}
	ix.ord++
	t := &tableState{name: name, ord: ix.ord}
	for _, s := range coreSuffixes {
		if strings.HasSuffix(name, s) {
			t.prefix, t.suffix = strings.TrimSuffix(name, s), s
			break
		}
	}
	ix.tabs[name] = t
	return t
}

func (ix *indexer) warnf(format string, args ...any) {
	if len(ix.warnings) < 100 {
		ix.warnings = append(ix.warnings, fmt.Sprintf(format, args...))
	}
}

func (ix *indexer) item(it *sqldump.Item) error {
	switch it.Kind {
	case sqldump.Statement:
		switch it.Type {
		case sqldump.StmtCreateTable:
			t := ix.table(it.Table)
			cols, err := sqldump.ParseColumns(it.Body)
			if err != nil {
				ix.warnf("table %s: %v", it.Table, err)
			}
			t.cols = cols
			t.createSQL = string(it.Body)
			t.otherBytes += int64(len(it.Raw))
		case sqldump.StmtDropTable:
			ix.table(it.Table).otherBytes += int64(len(it.Raw))
		case sqldump.StmtInsert:
			ix.warnf("offset %d: INSERT statement that is not a VALUES list was not indexed", it.Offset)
		}
	case sqldump.InsertHeader:
		ix.beginInsert(it)
	case sqldump.Row:
		return ix.row(it)
	case sqldump.InsertEnd:
		ix.cur.otherBytes += int64(len(it.Raw))
		ix.cur = nil
	}
	return nil
}

func (ix *indexer) beginInsert(it *sqldump.Item) {
	t := ix.table(it.Table)
	ix.cur = t
	t.otherBytes += int64(len(it.Raw))

	names := it.Columns
	if names == nil {
		if t.cols == nil {
			ix.warnf("table %s: INSERT without a column list and no CREATE TABLE before it; rows are counted but not indexed", t.name)
		}
		for _, c := range t.cols {
			names = append(names, c.Name)
		}
	}
	ix.ncols = len(names)
	find := func(col string) int {
		for i, n := range names {
			if strings.EqualFold(n, col) {
				return i
			}
		}
		return -1
	}

	ix.pos = ix.pos[:0]
	if want, ok := roleColumns[t.suffix]; ok {
		for _, c := range want {
			ix.pos = append(ix.pos, find(c))
		}
	}
	ix.objectPos, ix.objectCol = -1, ""
	if t.suffix == "" {
		for _, c := range []string{"post_id", "object_id"} {
			if i := find(c); i >= 0 {
				ix.objectPos, ix.objectCol = i, c
				break
			}
		}
	}
}

// --- field access for the current row ---

func (ix *indexer) field(n int) (sqldump.Field, bool) {
	if n >= len(ix.pos) || ix.pos[n] < 0 {
		return sqldump.Field{}, false
	}
	return ix.fields[ix.pos[n]], true
}

// id returns field n as an int64, or nil when missing, NULL, or not a number.
func (ix *indexer) id(n int) driver.Value {
	f, ok := ix.field(n)
	if !ok || f.IsNull() {
		return nil
	}
	v, err := f.Uint64()
	if err != nil {
		return nil
	}
	return int64(v)
}

// str returns field n decoded as a string, or nil.
func (ix *indexer) str(n int) driver.Value {
	b, ok := ix.bytes(n)
	if !ok {
		return nil
	}
	return string(b)
}

// bytes returns field n decoded into a scratch buffer that the next call reuses.
func (ix *indexer) bytes(n int) ([]byte, bool) {
	f, ok := ix.field(n)
	if !ok || f.IsNull() {
		return nil, false
	}
	b, err := f.AppendValue(ix.scratch[:0])
	if err != nil {
		return nil, false
	}
	ix.scratch = b
	return b, true
}

func (ix *indexer) date(n int) driver.Value {
	b, ok := ix.bytes(n)
	if !ok || len(b) < 10 || bytes.HasPrefix(b, []byte("0000")) {
		return nil
	}
	t, err := time.Parse(time.DateTime, string(b))
	if err != nil {
		return nil
	}
	return t
}

func (ix *indexer) appendRow(table string, args ...driver.Value) error {
	if err := ix.app[table].AppendRow(args...); err != nil {
		return fmt.Errorf("index: append to %s: %w", table, err)
	}
	return nil
}

// --- rows ---

func (ix *indexer) row(it *sqldump.Item) error {
	t := ix.cur
	t.rows++
	size := int64(len(it.Body))
	t.rowBytes += size
	ix.fields = it.Fields

	if len(it.Fields) != ix.ncols {
		if t.badRows == 0 && ix.ncols > 0 {
			ix.warnf("table %s: row with %d values, expected %d; such rows are counted but not indexed", t.name, len(it.Fields), ix.ncols)
		}
		t.badRows++
		return nil
	}

	if ix.objectPos >= 0 {
		f := it.Fields[ix.objectPos]
		if v, err := f.Uint64(); err == nil {
			return ix.appendRow("object_rows", t.name, ix.objectCol, int64(v), size)
		}
		return nil
	}

	switch t.suffix {
	case "posts":
		return ix.post(t, size)
	case "postmeta":
		if err := ix.appendRow("postmeta", t.name, ix.id(0), ix.id(1), ix.str(2), size); err != nil {
			return err
		}
		return ix.postmetaRefs(t)
	case "terms":
		return ix.appendRow("terms", t.name, ix.id(0), ix.str(2), ix.str(1), size)
	case "term_taxonomy":
		return ix.appendRow("term_taxonomy", t.name, ix.id(0), ix.id(1), ix.str(2), ix.id(3), ix.id(4), size)
	case "term_relationships":
		return ix.appendRow("term_relationships", t.name, ix.id(0), ix.id(1), size)
	case "termmeta":
		return ix.appendRow("termmeta", t.name, ix.id(0), ix.id(1), ix.str(2), size)
	case "users":
		return ix.appendRow("users", t.name, ix.id(0), size)
	case "usermeta":
		key := ix.str(2)
		if err := ix.appendRow("usermeta", t.name, ix.id(0), ix.id(1), key, size); err != nil {
			return err
		}
		if k, ok := key.(string); ok && strings.HasSuffix(k, "capabilities") {
			return ix.roles(t, ix.id(1), k)
		}
	case "comments":
		return ix.appendRow("comments", t.name, ix.id(0), ix.id(1), ix.str(2), ix.str(3), ix.id(4), ix.id(5), ix.date(6), size)
	case "commentmeta":
		return ix.appendRow("commentmeta", t.name, ix.id(0), ix.id(1), ix.str(2), size)
	case "options":
		name := ix.str(1)
		var value driver.Value
		if n, ok := name.(string); ok && keptOptionValues[n] {
			value = ix.str(3)
		}
		return ix.appendRow("options", t.name, ix.id(0), name, ix.str(2), size, value)
	}
	return nil
}

func (ix *indexer) post(t *tableState, size int64) error {
	id := ix.id(0)
	typ := ix.str(5)
	if err := ix.appendRow("posts", t.name, id, typ, ix.str(3), ix.id(1), ix.id(4), ix.date(2), ix.str(6), size); err != nil {
		return err
	}
	if id == nil {
		return nil
	}

	switch typ {
	case "revision":
		// Revisions are always dropped, so their references do not matter.
		return nil
	case "acf-field":
		return ix.acfField(t, id)
	}

	f, ok := ix.field(9)
	if !ok || !mayHaveContentRefs(f.Raw) {
		return nil
	}
	content, ok := ix.bytes(9)
	if !ok {
		return nil
	}
	ix.crefs = contentRefs(content, ix.crefs[:0])
	for _, r := range ix.crefs {
		if err := ix.appendRow("content_refs", t.name, id, r.id, r.source); err != nil {
			return err
		}
	}
	return nil
}

// acfField records an ACF field definition. ACF 5 and later store each field
// as an acf-field post: the key in post_name, the name in post_excerpt, and
// the settings, including the type, serialized in post_content.
func (ix *indexer) acfField(t *tableState, id driver.Value) error {
	key, name, parent := ix.str(7), ix.str(8), ix.id(4)
	var typ driver.Value
	if content, ok := ix.bytes(9); ok {
		if v, err := phpser.Unserialize(content); err == nil {
			if tv, ok := v.Lookup("type"); ok && tv.Kind == phpser.String {
				typ = string(tv.Str)
			}
		}
	}
	return ix.appendRow("acf_fields", t.name, id, key, name, typ, parent)
}

func (ix *indexer) postmetaRefs(t *tableState) error {
	f, ok := ix.field(3)
	if !ok || len(f.Raw) > maxRefValueBytes {
		return nil
	}
	switch f.Type {
	case sqldump.FieldNumber:
	case sqldump.FieldString:
		// Skip the decode unless the value starts like a number, a
		// serialized array, or a list.
		if len(f.Raw) < 3 {
			return nil
		}
		c := f.Raw[1]
		if !(c >= '0' && c <= '9' || c == 'a' || c == 'O' || c == '[' || c == ' ') {
			return nil
		}
	default:
		return nil
	}
	v, ok := ix.bytes(3)
	if !ok {
		return nil
	}
	ix.refs = metaRefs(v, ix.refs[:0])
	if len(ix.refs) == 0 {
		return nil
	}
	metaID, postID, key := ix.id(0), ix.id(1), ix.str(2)
	for _, ref := range ix.refs {
		if err := ix.appendRow("meta_refs", t.name, metaID, postID, key, ref); err != nil {
			return err
		}
	}
	return nil
}

// roles records the roles in a serialized capabilities value such as
// a:1:{s:13:"administrator";b:1;}.
func (ix *indexer) roles(t *tableState, userID driver.Value, key string) error {
	v, ok := ix.bytes(3)
	if !ok {
		return nil
	}
	pv, err := phpser.Unserialize(v)
	if err != nil || pv.Kind != phpser.Array {
		return nil
	}
	for _, e := range pv.Entries {
		if e.Key.Kind == phpser.String && (e.Val.Kind != phpser.Bool || e.Val.Int == 1) {
			if err := ix.appendRow("user_roles", t.name, userID, key, string(e.Key.Str)); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- after the stream ---

var subsitePrefix = regexp.MustCompile(`^(.*?)(\d+)_$`)

// finish picks the main table prefix, drops rows from other prefixes, and
// writes the tables and info rows.
func (ix *indexer) finish(ctx context.Context, db *sql.DB, src Source, sum *Summary) error {
	// A prefix is a candidate when it has both a posts and an options table.
	type cand struct {
		prefix string
		posts  int64
	}
	var cands []cand
	for _, t := range ix.tabs {
		if t.suffix != "posts" {
			continue
		}
		if _, ok := ix.tabs[t.prefix+"options"]; ok {
			cands = append(cands, cand{t.prefix, t.rows})
		}
	}
	if len(cands) == 0 {
		return errors.New("index: no WordPress tables found: the dump has no pair of posts and options tables with the same prefix")
	}
	isSubsite := func(p string) bool {
		m := subsitePrefix.FindStringSubmatch(p)
		if m == nil {
			return false
		}
		_, ok := ix.tabs[m[1]+"posts"]
		return ok
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if sa, sb := isSubsite(a.prefix), isSubsite(b.prefix); sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		if a.posts != b.posts {
			if a.posts > b.posts {
				return -1
			}
			return 1
		}
		return strings.Compare(a.prefix, b.prefix)
	})
	prefix := cands[0].prefix
	sum.Prefix = prefix

	subsites := 0
	for _, c := range cands[1:] {
		if isSubsite(c.prefix) {
			subsites++
		} else {
			ix.warnf("tables with prefix %q also look like WordPress; only prefix %q is indexed", c.prefix, prefix)
		}
	}
	_, hasBlogs := ix.tabs[prefix+"blogs"]
	sum.Multisite = subsites > 0 || hasBlogs
	if sum.Multisite {
		ix.warnf("multisite detected (%d subsite table sets); only the main site, prefix %q, is indexed", subsites, prefix)
	}

	for _, t := range rowTables {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tbl <> ?`, t.table), prefix+t.suffix); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(ix.tabs))
	for name := range ix.tabs {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int { return ix.tabs[a].ord - ix.tabs[b].ord })
	for _, name := range names {
		t := ix.tabs[name]
		role := ""
		if t.suffix != "" && t.prefix == prefix {
			role = t.suffix
		}
		cols := make([]string, len(t.cols))
		for i, c := range t.cols {
			cols[i] = c.Name
		}
		colsJSON, _ := json.Marshal(cols)
		if _, err := db.ExecContext(ctx, `INSERT INTO tables VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			name, role, t.ord, string(colsJSON), t.createSQL, t.rows, t.rowBytes, t.otherBytes); err != nil {
			return fmt.Errorf("index: write table %s: %w", name, err)
		}
		sum.Rows += t.rows
		if t.badRows > 0 {
			ix.warnf("table %s: %d rows had the wrong number of values and were not indexed", name, t.badRows)
		}
	}
	sum.Tables = len(ix.tabs)
	sum.Warnings = ix.warnings

	warnings, _ := json.Marshal(ix.warnings)
	info := map[string]string{
		"schema_version": schemaVersion,
		"source_path":    src.Path,
		"source_size":    strconv.FormatInt(src.Size, 10),
		"source_mtime":   src.ModTime.UTC().Format(time.RFC3339),
		"source_read":    strconv.FormatInt(sum.Bytes, 10),
		"indexed_at":     time.Now().UTC().Format(time.RFC3339),
		"prefix":         prefix,
		"multisite":      strconv.FormatBool(sum.Multisite),
		"warnings":       string(warnings),
	}
	for k, v := range info {
		if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO info VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}
