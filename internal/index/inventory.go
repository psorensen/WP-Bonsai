package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" driver
)

// Open opens an existing index read-only.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("duckdb", path+"?access_mode=READ_ONLY")
	if err != nil {
		return nil, err
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM info WHERE key = 'schema_version'`).Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: %s is not a bonsai index: %w", path, err)
	}
	if v != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("index: %s has schema version %s, this bonsai needs %s; run bonsai index again", path, v, schemaVersion)
	}
	return db, nil
}

// Inventory is what a dump contains, as bonsai inspect prints it.
type Inventory struct {
	Source     SourceInfo     `json:"source"`
	Prefix     string         `json:"prefix"`
	Multisite  bool           `json:"multisite"`
	PostTypes  []PostType     `json:"post_types"`
	Taxonomies []Taxonomy     `json:"taxonomies"`
	Tables     []Table        `json:"tables"`
	Options    OptionsInfo    `json:"options"`
	Users      UsersInfo      `json:"users"`
	Comments   CommentsInfo   `json:"comments"`
	ACFFields  map[string]int `json:"acf_field_types"`
	References References     `json:"references"`
	Warnings   []string       `json:"warnings"`
}

type SourceInfo struct {
	Path        string  `json:"path"`
	Bytes       int64   `json:"bytes"`
	ModTime     string  `json:"modified"`
	IndexedAt   string  `json:"indexed_at"`
	IndexSecond float64 `json:"index_seconds"`
}

type PostType struct {
	Type       string           `json:"type"`
	Count      int64            `json:"count"`
	Statuses   map[string]int64 `json:"statuses"`
	FirstDate  *string          `json:"first_date"`
	LastDate   *string          `json:"last_date"`
	AvgBytes   int64            `json:"avg_bytes"`
	TotalBytes int64            `json:"total_bytes"`
}

type Taxonomy struct {
	Taxonomy     string           `json:"taxonomy"`
	Terms        int64            `json:"terms"`
	Hierarchical bool             `json:"hierarchical"`
	PostTypes    map[string]int64 `json:"post_types"` // relationship count per post type
	LargestTerms []TermCount      `json:"largest_terms"`
}

type TermCount struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Posts int64  `json:"posts"`
}

type Table struct {
	Name     string `json:"name"`
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

type UsersInfo struct {
	Count int64            `json:"count"`
	Roles map[string]int64 `json:"roles"`
}

type CommentsInfo struct {
	Count    int64            `json:"count"`
	ByStatus map[string]int64 `json:"by_status"`
}

type References struct {
	// Content counts IDs found in post_content per source, and how many of
	// them point at a post in the dump.
	Content []RefCount `json:"content"`
	// MetaKeys lists the meta keys whose values most often hold the ID of
	// a post in the dump.
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
	inv := &Inventory{ACFFields: map[string]int{}}
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
	inv.Warnings = []string{}
	if w := info["warnings"]; w != "" && w != "null" {
		if err := json.Unmarshal([]byte(w), &inv.Warnings); err != nil {
			return nil, err
		}
	}

	// Post types.
	types := map[string]*PostType{}
	r.rows(`SELECT type, count(*), strftime(min(date), '%Y-%m-%d'), strftime(max(date), '%Y-%m-%d'),
			CAST(avg(bytes) AS BIGINT), sum(bytes)
		FROM posts GROUP BY type ORDER BY sum(bytes) DESC, type`, func(s scanner) error {
		pt := PostType{Statuses: map[string]int64{}}
		var typ sql.NullString
		if err := s.Scan(&typ, &pt.Count, &pt.FirstDate, &pt.LastDate, &pt.AvgBytes, &pt.TotalBytes); err != nil {
			return err
		}
		pt.Type = typ.String
		inv.PostTypes = append(inv.PostTypes, pt)
		types[pt.Type] = &inv.PostTypes[len(inv.PostTypes)-1]
		return nil
	})
	r.rows(`SELECT coalesce(type, ''), coalesce(status, ''), count(*) FROM posts GROUP BY ALL`, func(s scanner) error {
		var typ, status string
		var n int64
		if err := s.Scan(&typ, &status, &n); err != nil {
			return err
		}
		if pt := types[typ]; pt != nil {
			pt.Statuses[status] = n
		}
		return nil
	})

	// Taxonomies.
	taxes := map[string]*Taxonomy{}
	r.rows(`SELECT taxonomy, count(*), bool_or(parent > 0) FROM term_taxonomy GROUP BY taxonomy ORDER BY count(*) DESC, taxonomy`, func(s scanner) error {
		tx := Taxonomy{PostTypes: map[string]int64{}, LargestTerms: []TermCount{}}
		if err := s.Scan(&tx.Taxonomy, &tx.Terms, &tx.Hierarchical); err != nil {
			return err
		}
		inv.Taxonomies = append(inv.Taxonomies, tx)
		return nil
	})
	for i := range inv.Taxonomies {
		taxes[inv.Taxonomies[i].Taxonomy] = &inv.Taxonomies[i]
	}
	r.rows(`SELECT tt.taxonomy, coalesce(p.type, '(missing post)'), count(*)
		FROM term_relationships tr
		JOIN term_taxonomy tt USING (term_taxonomy_id)
		LEFT JOIN posts p ON p.id = tr.object_id
		GROUP BY ALL`, func(s scanner) error {
		var tax, typ string
		var n int64
		if err := s.Scan(&tax, &typ, &n); err != nil {
			return err
		}
		if tx := taxes[tax]; tx != nil {
			tx.PostTypes[typ] = n
		}
		return nil
	})
	r.rows(`WITH c AS (SELECT term_taxonomy_id, count(*) AS n FROM term_relationships GROUP BY 1)
		SELECT tt.taxonomy, coalesce(t.slug, ''), coalesce(t.name, ''), c.n
		FROM c JOIN term_taxonomy tt USING (term_taxonomy_id)
		LEFT JOIN terms t ON t.term_id = tt.term_id
		QUALIFY row_number() OVER (PARTITION BY tt.taxonomy ORDER BY c.n DESC, t.slug) <= 5
		ORDER BY tt.taxonomy, c.n DESC, t.slug`, func(s scanner) error {
		var tax string
		var tc TermCount
		if err := s.Scan(&tax, &tc.Slug, &tc.Name, &tc.Posts); err != nil {
			return err
		}
		if tx := taxes[tax]; tx != nil {
			tx.LargestTerms = append(tx.LargestTerms, tc)
		}
		return nil
	})

	// Tables.
	r.rows(`SELECT name, role, rows, row_bytes + other_bytes, row_bytes FROM tables ORDER BY row_bytes + other_bytes DESC, name`, func(s scanner) error {
		var t Table
		if err := s.Scan(&t.Name, &t.Role, &t.Rows, &t.Bytes, &t.RowBytes); err != nil {
			return err
		}
		inv.Tables = append(inv.Tables, t)
		return nil
	})

	// Options.
	o := &inv.Options
	o.Values = map[string]string{}
	o.Large = []NamedSize{}
	r.one(`SELECT count(*), coalesce(sum(bytes), 0),
			coalesce(sum(bytes) FILTER (WHERE autoload IN ('yes', 'on', 'auto', 'auto-on')), 0),
			count(*) FILTER (WHERE name LIKE '\_transient\_%' ESCAPE '\' OR name LIKE '\_site\_transient\_%' ESCAPE '\'),
			coalesce(sum(bytes) FILTER (WHERE name LIKE '\_transient\_%' ESCAPE '\' OR name LIKE '\_site\_transient\_%' ESCAPE '\'), 0)
		FROM options`, &o.Count, &o.Bytes, &o.AutoloadBytes, &o.TransientCount, &o.TransientBytes)
	r.rows(`SELECT name, bytes FROM options WHERE bytes > ? ORDER BY bytes DESC`, func(s scanner) error {
		var ns NamedSize
		if err := s.Scan(&ns.Name, &ns.Bytes); err != nil {
			return err
		}
		o.Large = append(o.Large, ns)
		return nil
	}, largeOptionBytes)
	r.rows(`SELECT name, value FROM options WHERE value IS NOT NULL ORDER BY name`, func(s scanner) error {
		var k, v string
		if err := s.Scan(&k, &v); err != nil {
			return err
		}
		o.Values[k] = v
		return nil
	})
	if v, err := strconv.ParseInt(o.Values["posts_per_page"], 10, 64); err == nil {
		o.PostsPerPage = &v
	}

	// Users and comments.
	inv.Users.Roles = map[string]int64{}
	r.one(`SELECT count(*) FROM users`, &inv.Users.Count)
	r.rows(`SELECT role, count(DISTINCT user_id) FROM user_roles WHERE meta_key = ? GROUP BY role`, func(s scanner) error {
		var role string
		var n int64
		if err := s.Scan(&role, &n); err != nil {
			return err
		}
		inv.Users.Roles[role] = n
		return nil
	}, inv.Prefix+"capabilities")
	inv.Comments.ByStatus = map[string]int64{}
	r.one(`SELECT count(*) FROM comments`, &inv.Comments.Count)
	r.rows(`SELECT coalesce(approved, ''), count(*) FROM comments GROUP BY 1`, func(s scanner) error {
		var st string
		var n int64
		if err := s.Scan(&st, &n); err != nil {
			return err
		}
		inv.Comments.ByStatus[commentStatus(st)] += n
		return nil
	})

	// ACF.
	r.rows(`SELECT coalesce(field_type, 'unknown'), count(*) FROM acf_fields GROUP BY 1`, func(s scanner) error {
		var typ string
		var n int
		if err := s.Scan(&typ, &n); err != nil {
			return err
		}
		inv.ACFFields[typ] = n
		return nil
	})

	// References.
	inv.References.Content = []RefCount{}
	inv.References.MetaKeys = []RefCount{}
	r.rows(`SELECT source, count(*), count(p.id) FROM content_refs c LEFT JOIN posts p ON p.id = c.ref_id
		GROUP BY source ORDER BY count(*) DESC, source`, func(s scanner) error {
		var rc RefCount
		if err := s.Scan(&rc.Name, &rc.Refs, &rc.Resolved); err != nil {
			return err
		}
		inv.References.Content = append(inv.References.Content, rc)
		return nil
	})
	r.rows(`SELECT meta_key, count(*), count(p.id) FROM meta_refs m LEFT JOIN posts p ON p.id = m.ref_id
		GROUP BY meta_key HAVING count(p.id) > 0 ORDER BY count(p.id) DESC, meta_key LIMIT 30`, func(s scanner) error {
		var rc RefCount
		if err := s.Scan(&rc.Name, &rc.Refs, &rc.Resolved); err != nil {
			return err
		}
		inv.References.MetaKeys = append(inv.References.MetaKeys, rc)
		return nil
	})

	if r.err != nil {
		return nil, r.err
	}
	return inv, nil
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

func (r *reader) one(query string, dest ...any) {
	if r.err != nil {
		return
	}
	if err := r.db.QueryRowContext(r.ctx, query).Scan(dest...); err != nil {
		r.err = fmt.Errorf("index: %w\nquery: %s", err, query)
	}
}
