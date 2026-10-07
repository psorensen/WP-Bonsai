package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" driver
)

// Open opens an existing index read-only.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("duckdb", path+"?access_mode=READ_ONLY")
	if err != nil {
		return nil, err
	}
	if err := CheckVersion(db, ""); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: %s: %w", path, err)
	}
	return db, nil
}

// CheckVersion checks that the index in schema (empty for the default
// catalog, or the name of an attached database) has this program's schema.
func CheckVersion(db *sql.DB, schema string) error {
	if schema != "" {
		schema += "."
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM ` + schema + `info WHERE key = 'schema_version'`).Scan(&v); err != nil {
		return fmt.Errorf("not a bonsai index: %w", err)
	}
	if v != schemaVersion {
		return fmt.Errorf("index has schema version %s, this bonsai needs %s; run bonsai index again", v, schemaVersion)
	}
	return nil
}

// ReadSource returns the dump an index was built from.
func ReadSource(db *sql.DB) (Source, error) {
	var src Source
	var size, mtime string
	if err := db.QueryRow(`SELECT
			coalesce(max(value) FILTER (WHERE key = 'source_path'), ''),
			coalesce(max(value) FILTER (WHERE key = 'source_size'), ''),
			coalesce(max(value) FILTER (WHERE key = 'source_mtime'), '')
		FROM info`).Scan(&src.Path, &size, &mtime); err != nil {
		return src, err
	}
	src.Size, _ = strconv.ParseInt(size, 10, 64)
	src.ModTime, _ = time.Parse(time.RFC3339, mtime)
	return src, nil
}

// Matches reports whether an index source describes the same file as s,
// by size and modification time to the second.
func (s Source) Matches(o Source) bool {
	return s.Size == o.Size && s.ModTime.UTC().Truncate(time.Second).Equal(o.ModTime.UTC().Truncate(time.Second))
}

// Inventory is what a dump contains, as bonsai inspect prints it.
type Inventory struct {
	Source    SourceInfo `json:"source"`
	Prefix    string     `json:"prefix"`
	Multisite bool       `json:"multisite"`
	Users     int64      `json:"users"`
	Sites     []Site     `json:"sites"`
	Tables    []Table    `json:"tables"`
	Warnings  []string   `json:"warnings"`
}

type SourceInfo struct {
	Path        string  `json:"path"`
	Bytes       int64   `json:"bytes"`
	ModTime     string  `json:"modified"`
	IndexedAt   string  `json:"indexed_at"`
	IndexSecond float64 `json:"index_seconds"`
}

// Site is the inventory of one site. A single-site install has one, blog 1.
type Site struct {
	BlogID     int              `json:"blog_id"`
	Prefix     string           `json:"prefix"`
	Domain     string           `json:"domain,omitempty"`
	Path       string           `json:"path,omitempty"`
	Archived   bool             `json:"archived,omitempty"`
	Deleted    bool             `json:"deleted,omitempty"`
	Spam       bool             `json:"spam,omitempty"`
	Bytes      int64            `json:"bytes"` // all tables of this site
	PostTypes  []PostType       `json:"post_types"`
	Taxonomies []Taxonomy       `json:"taxonomies"`
	Options    OptionsInfo      `json:"options"`
	Roles      map[string]int64 `json:"roles"`
	Comments   CommentsInfo     `json:"comments"`
	ACFFields  map[string]int   `json:"acf_field_types"`
	References References       `json:"references"`
}

type PostType struct {
	Type       string           `json:"type"`
	Count      int64            `json:"count"`
	Statuses   map[string]int64 `json:"statuses"`
	FirstDate  *string          `json:"first_date"`
	LastDate   *string          `json:"last_date"`
	AvgBytes   int64            `json:"avg_bytes"`
	TotalBytes int64            `json:"total_bytes"`
	// MetaBytes is the size of the post meta of posts of this type.
	MetaBytes int64 `json:"meta_bytes"`
}

type Taxonomy struct {
	Taxonomy     string           `json:"taxonomy"`
	Terms        int64            `json:"terms"`
	Hierarchical bool             `json:"hierarchical"`
	PostTypes    map[string]int64 `json:"post_types"` // relationship count per post type
	// MenuTerms is how many terms of this taxonomy the site's menus link to.
	MenuTerms    int64       `json:"menu_terms"`
	LargestTerms []TermCount `json:"largest_terms"`
}

type TermCount struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Posts int64  `json:"posts"`
}

type Table struct {
	Name     string `json:"name"`
	Site     *int   `json:"site"` // nil for network and unplaced tables
	Role     string `json:"role"`
	Rows     int64  `json:"rows"`
	Bytes    int64  `json:"bytes"`
	RowBytes int64  `json:"row_bytes"`
}

type OptionsInfo struct {
	Count          int64             `json:"count"`
	Bytes          int64             `json:"bytes"`
	AutoloadBytes  int64             `json:"autoload_bytes"`
	TransientCount int64             `json:"transient_count"`
	TransientBytes int64             `json:"transient_bytes"`
	PostsPerPage   *int64            `json:"posts_per_page"`
	Large          []NamedSize       `json:"over_100kb"`
	Values         map[string]string `json:"values"`
}

type NamedSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

type CommentsInfo struct {
	Count    int64            `json:"count"`
	ByStatus map[string]int64 `json:"by_status"`
}

type References struct {
	// Content counts IDs found in post_content per source, and how many of
	// them point at a post of the same site.
	Content []RefCount `json:"content"`
	// MetaKeys lists the meta keys whose values most often hold the ID of
	// a post of the same site. Many matches are numbers that only look like
	// IDs, so use this list to choose references.extra_meta_keys.
	MetaKeys []RefCount `json:"meta_keys"`
}

type RefCount struct {
	Name     string `json:"name"`
	Refs     int64  `json:"refs"`
	Resolved int64  `json:"resolved"`
}

// largeOptionBytes is the size above which an option is listed. See SPEC.md.
const largeOptionBytes = 100 * 1024

// ReadInventory reads the inventory of an open index.
func ReadInventory(ctx context.Context, db *sql.DB) (*Inventory, error) {
	inv := &Inventory{Warnings: []string{}, Sites: []Site{}, Tables: []Table{}}
	r := reader{ctx: ctx, db: db}

	info := map[string]string{}
	r.rows(`SELECT key, value FROM info`, func(s scanner) error {
		var k, v string
		if err := s.Scan(&k, &v); err != nil {
			return err
		}
		info[k] = v
		return nil
	})
	inv.Prefix = info["prefix"]
	inv.Multisite = info["multisite"] == "true"
	inv.Source.Path = info["source_path"]
	inv.Source.Bytes, _ = strconv.ParseInt(info["source_read"], 10, 64)
	inv.Source.ModTime = info["source_mtime"]
	inv.Source.IndexedAt = info["indexed_at"]
	inv.Source.IndexSecond, _ = strconv.ParseFloat(info["duration_seconds"], 64)
	if w := info["warnings"]; w != "" && w != "null" {
		if err := json.Unmarshal([]byte(w), &inv.Warnings); err != nil {
			return nil, err
		}
	}
	r.one(`SELECT count(*) FROM users`, nil, &inv.Users)

	r.rows(`SELECT name, site, role, rows, row_bytes + other_bytes, row_bytes FROM tables ORDER BY row_bytes + other_bytes DESC, name`, func(s scanner) error {
		var t Table
		var site sql.NullInt64
		if err := s.Scan(&t.Name, &site, &t.Role, &t.Rows, &t.Bytes, &t.RowBytes); err != nil {
			return err
		}
		if site.Valid {
			n := int(site.Int64)
			t.Site = &n
		}
		inv.Tables = append(inv.Tables, t)
		return nil
	})

	r.rows(`SELECT s.blog_id, s.prefix, coalesce(s.domain, ''), coalesce(s.path, ''),
			coalesce(s.archived, false), coalesce(s.deleted, false), coalesce(s.spam, false),
			(SELECT coalesce(sum(row_bytes + other_bytes), 0) FROM tables t WHERE t.site = s.blog_id)
		FROM sites s ORDER BY s.blog_id`, func(s scanner) error {
		var st Site
		if err := s.Scan(&st.BlogID, &st.Prefix, &st.Domain, &st.Path, &st.Archived, &st.Deleted, &st.Spam, &st.Bytes); err != nil {
			return err
		}
		inv.Sites = append(inv.Sites, st)
		return nil
	})
	for i := range inv.Sites {
		r.site(&inv.Sites[i])
	}
	if r.err != nil {
		return nil, r.err
	}
	return inv, nil
}

// site reads the inventory of one site.
func (r *reader) site(st *Site) {
	id := st.BlogID
	st.PostTypes = []PostType{}
	st.Taxonomies = []Taxonomy{}
	st.Roles = map[string]int64{}
	st.ACFFields = map[string]int{}

	// Post types.
	types := map[string]int{}
	r.rows(`WITH meta AS (
			SELECT post_id, sum(bytes) AS bytes FROM postmeta WHERE site = $1 GROUP BY post_id
		)
		SELECT coalesce(p.type, ''), count(*), strftime(min(p.date), '%Y-%m-%d'), strftime(max(p.date), '%Y-%m-%d'),
			CAST(avg(p.bytes) AS BIGINT), sum(p.bytes), coalesce(sum(m.bytes), 0)
		FROM posts p LEFT JOIN meta m ON m.post_id = p.id
		WHERE p.site = $1
		GROUP BY 1 ORDER BY sum(p.bytes) + coalesce(sum(m.bytes), 0) DESC, 1`, func(s scanner) error {
		pt := PostType{Statuses: map[string]int64{}}
		if err := s.Scan(&pt.Type, &pt.Count, &pt.FirstDate, &pt.LastDate, &pt.AvgBytes, &pt.TotalBytes, &pt.MetaBytes); err != nil {
			return err
		}
		types[pt.Type] = len(st.PostTypes)
		st.PostTypes = append(st.PostTypes, pt)
		return nil
	}, id)
	r.rows(`SELECT coalesce(type, ''), coalesce(status, ''), count(*) FROM posts WHERE site = $1 GROUP BY ALL`, func(s scanner) error {
		var typ, status string
		var n int64
		if err := s.Scan(&typ, &status, &n); err != nil {
			return err
		}
		if i, ok := types[typ]; ok {
			st.PostTypes[i].Statuses[status] = n
		}
		return nil
	}, id)

	// Taxonomies.
	taxes := map[string]int{}
	r.rows(`SELECT taxonomy, count(*), bool_or(parent > 0) FROM term_taxonomy WHERE site = $1
		GROUP BY taxonomy ORDER BY count(*) DESC, taxonomy`, func(s scanner) error {
		tx := Taxonomy{PostTypes: map[string]int64{}, LargestTerms: []TermCount{}}
		if err := s.Scan(&tx.Taxonomy, &tx.Terms, &tx.Hierarchical); err != nil {
			return err
		}
		taxes[tx.Taxonomy] = len(st.Taxonomies)
		st.Taxonomies = append(st.Taxonomies, tx)
		return nil
	}, id)
	r.rows(`SELECT tt.taxonomy, coalesce(p.type, '(missing post)'), count(*)
		FROM term_relationships tr
		JOIN term_taxonomy tt ON tt.site = tr.site AND tt.term_taxonomy_id = tr.term_taxonomy_id
		LEFT JOIN posts p ON p.site = tr.site AND p.id = tr.object_id
		WHERE tr.site = $1
		GROUP BY ALL`, func(s scanner) error {
		var tax, typ string
		var n int64
		if err := s.Scan(&tax, &typ, &n); err != nil {
			return err
		}
		if i, ok := taxes[tax]; ok {
			st.Taxonomies[i].PostTypes[typ] = n
		}
		return nil
	}, id)
	r.rows(`SELECT ob.value, count(DISTINCT o.value) FROM menu_meta o
		JOIN menu_meta ty ON ty.site = o.site AND ty.post_id = o.post_id AND ty.meta_key = '_menu_item_type' AND ty.value = 'taxonomy'
		JOIN menu_meta ob ON ob.site = o.site AND ob.post_id = o.post_id AND ob.meta_key = '_menu_item_object'
		WHERE o.site = $1 AND o.meta_key = '_menu_item_object_id' GROUP BY 1`, func(s scanner) error {
		var tax string
		var n int64
		if err := s.Scan(&tax, &n); err != nil {
			return err
		}
		if i, ok := taxes[tax]; ok {
			st.Taxonomies[i].MenuTerms = n
		}
		return nil
	}, id)
	r.rows(`WITH c AS (
			SELECT term_taxonomy_id, count(*) AS n FROM term_relationships WHERE site = $1 GROUP BY 1
		)
		SELECT tt.taxonomy, coalesce(t.slug, ''), coalesce(t.name, ''), c.n
		FROM c JOIN term_taxonomy tt ON tt.site = $1 AND tt.term_taxonomy_id = c.term_taxonomy_id
		LEFT JOIN terms t ON t.site = $1 AND t.term_id = tt.term_id
		QUALIFY row_number() OVER (PARTITION BY tt.taxonomy ORDER BY c.n DESC, t.slug) <= 5
		ORDER BY tt.taxonomy, c.n DESC, t.slug`, func(s scanner) error {
		var tax string
		var tc TermCount
		if err := s.Scan(&tax, &tc.Slug, &tc.Name, &tc.Posts); err != nil {
			return err
		}
		if i, ok := taxes[tax]; ok {
			st.Taxonomies[i].LargestTerms = append(st.Taxonomies[i].LargestTerms, tc)
		}
		return nil
	}, id)

	// Options.
	o := &st.Options
	o.Values = map[string]string{}
	o.Large = []NamedSize{}
	r.one(`SELECT count(*), coalesce(sum(bytes), 0),
			coalesce(sum(bytes) FILTER (WHERE autoload IN ('yes', 'on', 'auto', 'auto-on')), 0),
			count(*) FILTER (WHERE name LIKE '\_transient\_%' ESCAPE '\' OR name LIKE '\_site\_transient\_%' ESCAPE '\'),
			coalesce(sum(bytes) FILTER (WHERE name LIKE '\_transient\_%' ESCAPE '\' OR name LIKE '\_site\_transient\_%' ESCAPE '\'), 0)
		FROM options WHERE site = $1`, []any{id}, &o.Count, &o.Bytes, &o.AutoloadBytes, &o.TransientCount, &o.TransientBytes)
	r.rows(`SELECT name, bytes FROM options WHERE site = $1 AND bytes > $2 ORDER BY bytes DESC`, func(s scanner) error {
		var ns NamedSize
		if err := s.Scan(&ns.Name, &ns.Bytes); err != nil {
			return err
		}
		o.Large = append(o.Large, ns)
		return nil
	}, id, largeOptionBytes)
	r.rows(`SELECT name, value FROM options WHERE site = $1 AND value IS NOT NULL ORDER BY name`, func(s scanner) error {
		var k, v string
		if err := s.Scan(&k, &v); err != nil {
			return err
		}
		o.Values[k] = v
		return nil
	}, id)
	if v, err := strconv.ParseInt(o.Values["posts_per_page"], 10, 64); err == nil {
		o.PostsPerPage = &v
	}

	// Roles, comments, ACF.
	r.rows(`SELECT role, count(DISTINCT user_id) FROM user_roles WHERE site = $1 GROUP BY role`, func(s scanner) error {
		var role string
		var n int64
		if err := s.Scan(&role, &n); err != nil {
			return err
		}
		st.Roles[role] = n
		return nil
	}, id)
	st.Comments.ByStatus = map[string]int64{}
	r.rows(`SELECT coalesce(approved, ''), count(*) FROM comments WHERE site = $1 GROUP BY 1`, func(s scanner) error {
		var status string
		var n int64
		if err := s.Scan(&status, &n); err != nil {
			return err
		}
		st.Comments.ByStatus[commentStatus(status)] += n
		st.Comments.Count += n
		return nil
	}, id)
	r.rows(`SELECT coalesce(field_type, 'unknown'), count(*) FROM acf_fields WHERE site = $1 GROUP BY 1`, func(s scanner) error {
		var typ string
		var n int
		if err := s.Scan(&typ, &n); err != nil {
			return err
		}
		st.ACFFields[typ] = n
		return nil
	}, id)

	// References.
	st.References.Content = []RefCount{}
	st.References.MetaKeys = []RefCount{}
	r.rows(`SELECT c.source, count(*), count(p.id) FROM content_refs c
		LEFT JOIN posts p ON p.site = c.site AND p.id = c.ref_id
		WHERE c.site = $1 GROUP BY c.source ORDER BY count(*) DESC, c.source`, func(s scanner) error {
		var rc RefCount
		if err := s.Scan(&rc.Name, &rc.Refs, &rc.Resolved); err != nil {
			return err
		}
		st.References.Content = append(st.References.Content, rc)
		return nil
	}, id)
	r.rows(`SELECT m.meta_key, count(*), count(p.id) FROM meta_refs m
		LEFT JOIN posts p ON p.site = m.site AND p.id = m.ref_id
		WHERE m.site = $1
		GROUP BY m.meta_key HAVING count(p.id) > 0 ORDER BY count(p.id) DESC, m.meta_key LIMIT 30`, func(s scanner) error {
		var rc RefCount
		if err := s.Scan(&rc.Name, &rc.Refs, &rc.Resolved); err != nil {
			return err
		}
		st.References.MetaKeys = append(st.References.MetaKeys, rc)
		return nil
	}, id)
}

func commentStatus(approved string) string {
	switch approved {
	case "1":
		return "approved"
	case "0":
		return "pending"
	}
	return approved
}

// reader runs queries and keeps the first error, so ReadInventory can list
// its queries without an error check after each one.
type reader struct {
	ctx context.Context
	db  *sql.DB
	err error
}

type scanner interface{ Scan(dest ...any) error }

func (r *reader) rows(query string, fn func(scanner) error, args ...any) {
	if r.err != nil {
		return
	}
	rows, err := r.db.QueryContext(r.ctx, query, args...)
	if err != nil {
		r.err = fmt.Errorf("index: %w\nquery: %s", err, query)
		return
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			r.err = fmt.Errorf("index: %w\nquery: %s", err, query)
			return
		}
	}
	if err := rows.Err(); err != nil {
		r.err = fmt.Errorf("index: %w\nquery: %s", err, query)
	}
}

func (r *reader) one(query string, args []any, dest ...any) {
	if r.err != nil {
		return
	}
	if err := r.db.QueryRowContext(r.ctx, query, args...).Scan(dest...); err != nil {
		r.err = fmt.Errorf("index: %w\nquery: %s", err, query)
	}
}
