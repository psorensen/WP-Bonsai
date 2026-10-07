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
	"maps"
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

// MemoryLimit returns the cap on the memory DuckDB may use, from
// BONSAI_DUCKDB_MEMORY, such as 1GB. The default is 256MB: on a 27 GB dump
// with 91 million rows, pass 1 took the same time with 256MB as with 1GB,
// and its peak memory fell from 1.7 GB to 0.7 GB. DuckDB writes to disk
// when it needs more.
func MemoryLimit() string {
	if v := os.Getenv("BONSAI_DUCKDB_MEMORY"); v != "" {
		return v
	}
	return "256MB"
}

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
	Sites     int
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
	"blogs":              {"blog_id", "domain", "path", "public", "archived", "deleted", "spam"},
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
	connector, err := duckdb.NewConnector(dbPath+"?memory_limit="+MemoryLimit(), nil)
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
	var names []string
	for _, t := range append(append(siteTables[:len(siteTables):len(siteTables)], networkTables...), struct{ table, suffix string }{"object_rows", ""}) {
		names = append(names, t.table)
	}
	for _, name := range names {
		a, err := duckdb.NewAppenderFromConn(conn, "", name)
		if err != nil {
			return 0, fmt.Errorf("index: appender for %s: %w", name, err)
		}
		ix.app[name] = a
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
	// A table is read as a core table only when it has the core table's ID
	// column. Plugin tables whose names end like a core table, such as
	// wp_yoast_seo_links, are read like any other plugin table.
	if len(ix.pos) == 0 || ix.pos[0] < 0 {
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
			return ix.appendRow("object_rows", t.name, nil, ix.objectCol, int64(v), size)
		}
		return nil
	}

	if len(ix.pos) == 0 || ix.pos[0] < 0 {
		return nil
	}
	switch t.suffix {
	case "posts":
		return ix.post(t, size)
	case "postmeta":
		if err := ix.appendRow("postmeta", t.name, nil, ix.id(0), ix.id(1), ix.str(2), size); err != nil {
			return err
		}
		return ix.postmetaRefs(t)
	case "terms":
		return ix.appendRow("terms", t.name, nil, ix.id(0), ix.str(2), ix.str(1), size)
	case "term_taxonomy":
		return ix.appendRow("term_taxonomy", t.name, nil, ix.id(0), ix.id(1), ix.str(2), ix.id(3), ix.id(4), size)
	case "term_relationships":
		return ix.appendRow("term_relationships", t.name, nil, ix.id(0), ix.id(1), size)
	case "termmeta":
		return ix.appendRow("termmeta", t.name, nil, ix.id(0), ix.id(1), ix.str(2), size)
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
		return ix.appendRow("comments", t.name, nil, ix.id(0), ix.id(1), ix.str(2), ix.str(3), ix.id(4), ix.id(5), ix.date(6), size)
	case "commentmeta":
		return ix.appendRow("commentmeta", t.name, nil, ix.id(0), ix.id(1), ix.str(2), size)
	case "options":
		name := ix.str(1)
		var value driver.Value
		if n, ok := name.(string); ok && keptOptionValues[n] {
			value = ix.str(3)
		}
		return ix.appendRow("options", t.name, nil, ix.id(0), name, ix.str(2), size, value)
	case "blogs":
		return ix.appendRow("blogs", t.name, ix.id(0), ix.str(1), ix.str(2), ix.str(3), ix.str(4), ix.str(5), ix.str(6))
	}
	return nil
}

func (ix *indexer) post(t *tableState, size int64) error {
	id := ix.id(0)
	typ := ix.str(5)
	if err := ix.appendRow("posts", t.name, nil, id, typ, ix.str(3), ix.id(1), ix.id(4), ix.date(2), ix.str(6), size); err != nil {
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
		if err := ix.appendRow("content_refs", t.name, nil, id, r.id, r.source); err != nil {
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
	return ix.appendRow("acf_fields", t.name, nil, id, key, name, typ, parent)
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
		if err := ix.appendRow("meta_refs", t.name, nil, metaID, postID, key, ref); err != nil {
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
			if err := ix.appendRow("user_roles", t.name, nil, userID, key, string(e.Key.Str)); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- after the stream ---

// sitePrefixRe matches a subsite prefix such as wp_12_ and captures the main
// prefix and the blog ID.
var sitePrefixRe = regexp.MustCompile(`^(.*?)(\d+)_$`)

// finish works out which tables belong to which site, fills in the site
// column, drops rows from prefixes that are not part of this install, and
// writes the sites, tables, and info rows.
func (ix *indexer) finish(ctx context.Context, db *sql.DB, src Source, sum *Summary) error {
	// A prefix is a candidate site when it has both a posts and an options table.
	posts := map[string]int64{}
	for _, t := range ix.tabs {
		if t.suffix == "posts" {
			if _, ok := ix.tabs[t.prefix+"options"]; ok {
				posts[t.prefix] = t.rows
			}
		}
	}
	if len(posts) == 0 {
		return errors.New("index: no WordPress tables found: the dump has no pair of posts and options tables with the same prefix")
	}
	// The main prefix is a candidate that is not a subsite of another
	// candidate. With several, the one with the most posts wins.
	subsiteOf := func(p string) (string, int, bool) {
		m := sitePrefixRe.FindStringSubmatch(p)
		if m == nil {
			return "", 0, false
		}
		if _, ok := posts[m[1]]; !ok {
			return "", 0, false
		}
		n, err := strconv.Atoi(m[2])
		return m[1], n, err == nil && n > 1
	}
	var mains []string
	for p := range posts {
		if _, _, ok := subsiteOf(p); !ok {
			mains = append(mains, p)
		}
	}
	slices.SortFunc(mains, func(a, b string) int {
		if posts[a] != posts[b] {
			if posts[a] > posts[b] {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	if len(mains) == 0 {
		return errors.New("index: could not tell the main table prefix from the subsite prefixes")
	}
	prefix := mains[0]
	sum.Prefix = prefix
	for _, other := range mains[1:] {
		ix.warnf("tables with prefix %q also look like WordPress; only prefix %q is indexed", other, prefix)
	}

	// Blog ID per site prefix.
	siteOf := map[string]int{prefix: 1}
	for p := range posts {
		if main, n, ok := subsiteOf(p); ok && main == prefix {
			siteOf[p] = n
		}
	}
	_, hasBlogs := ix.tabs[prefix+"blogs"]
	sum.Multisite = len(siteOf) > 1 || hasBlogs
	sum.Sites = len(siteOf)

	// Site and role of every table. A plugin table belongs to the site whose
	// prefix it starts with, the longest prefix first.
	sitePrefixes := slices.Collect(maps.Keys(siteOf))
	slices.SortFunc(sitePrefixes, func(a, b string) int { return len(b) - len(a) })
	type placed struct {
		site sql.NullInt64
		role string
	}
	place := map[string]placed{}
	for name, t := range ix.tabs {
		var pl placed
		if n, ok := siteOf[t.prefix]; ok && slices.Contains(siteSuffixes, t.suffix) {
			pl = placed{sql.NullInt64{Int64: int64(n), Valid: true}, t.suffix}
		} else if t.prefix == prefix && slices.Contains(networkSuffixes, t.suffix) {
			pl = placed{role: t.suffix}
		} else {
			for _, p := range sitePrefixes {
				if strings.HasPrefix(name, p) {
					pl.site = sql.NullInt64{Int64: int64(siteOf[p]), Valid: true}
					break
				}
			}
		}
		place[name] = pl
	}

	exec := func(q string, args ...any) error {
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("index: %w\nquery: %s", err, q)
		}
		return nil
	}
	if err := exec(`CREATE TEMP TABLE core_map (tbl VARCHAR, site INTEGER)`); err != nil {
		return err
	}
	if err := exec(`CREATE TEMP TABLE any_map (tbl VARCHAR, site INTEGER)`); err != nil {
		return err
	}
	for name, pl := range place {
		if !pl.site.Valid {
			continue
		}
		if pl.role != "" {
			if err := exec(`INSERT INTO core_map VALUES (?, ?)`, name, pl.site.Int64); err != nil {
				return err
			}
		}
		if err := exec(`INSERT INTO any_map VALUES (?, ?)`, name, pl.site.Int64); err != nil {
			return err
		}
	}
	for _, t := range siteTables {
		if err := exec(fmt.Sprintf(`UPDATE %[1]s SET site = m.site FROM core_map m
			WHERE %[1]s.tbl = m.tbl AND m.tbl LIKE '%%' || ?`, t.table), t.suffix); err != nil {
			return err
		}
		if err := exec(fmt.Sprintf(`DELETE FROM %s WHERE site IS NULL`, t.table)); err != nil {
			return err
		}
	}
	if err := exec(`UPDATE object_rows SET site = m.site FROM any_map m WHERE object_rows.tbl = m.tbl`); err != nil {
		return err
	}
	for _, t := range networkTables {
		if err := exec(fmt.Sprintf(`DELETE FROM %s WHERE tbl <> ?`, t.table), prefix+t.suffix); err != nil {
			return err
		}
	}
	// Roles apply to the site named by the capabilities key: wp_capabilities
	// for blog 1, wp_2_capabilities for blog 2.
	if err := exec(`CREATE TEMP TABLE cap_map (meta_key VARCHAR, site INTEGER)`); err != nil {
		return err
	}
	for p, n := range siteOf {
		if err := exec(`INSERT INTO cap_map VALUES (?, ?)`, p+"capabilities", n); err != nil {
			return err
		}
	}
	if err := exec(`UPDATE user_roles SET site = m.site FROM cap_map m WHERE user_roles.meta_key = m.meta_key`); err != nil {
		return err
	}
	if err := exec(`DELETE FROM user_roles WHERE site IS NULL`); err != nil {
		return err
	}

	// Sites: every row of wp_blogs, plus any site with tables but no row.
	if err := exec(`INSERT INTO sites
		SELECT blog_id, CASE WHEN blog_id = 1 THEN ? ELSE ? || blog_id || '_' END,
			domain, path, public = '1', archived = '1', deleted = '1', spam = '1'
		FROM blogs WHERE blog_id IS NOT NULL`, prefix, prefix); err != nil {
		return err
	}
	for p, n := range siteOf {
		if err := exec(`INSERT INTO sites (blog_id, prefix) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM sites WHERE blog_id = ?)`, n, p, n); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(ix.tabs))
	for name := range ix.tabs {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int { return ix.tabs[a].ord - ix.tabs[b].ord })
	for _, name := range names {
		t, pl := ix.tabs[name], place[name]
		cols := make([]string, len(t.cols))
		for i, c := range t.cols {
			cols[i] = c.Name
		}
		colsJSON, _ := json.Marshal(cols)
		if err := exec(`INSERT INTO tables VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			name, pl.site, pl.role, t.ord, string(colsJSON), t.createSQL, t.rows, t.rowBytes, t.otherBytes); err != nil {
			return err
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
		if err := exec(`INSERT OR REPLACE INTO info VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}
