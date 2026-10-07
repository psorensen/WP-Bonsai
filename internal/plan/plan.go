// Package plan builds the keep set: which rows of each table go into the
// slim dump. It runs SQL against the pass 1 index and estimates the output
// size from the row sizes pass 1 stored. See SPEC.md, "Keep-set rules".
//
// Every site of a multisite network gets its own keep set, keyed by blog ID.
// The keep set lives in an in-memory DuckDB database that attaches the index
// read-only, so planning never changes the index.
package plan

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2" // registers the "duckdb" driver

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/phpser"
)

// Plan is a keep set with its size estimate.
type Plan struct {
	Prefix        string         `json:"prefix"`
	TargetBytes   int64          `json:"target_bytes"`
	EstimateBytes int64          `json:"estimate_bytes"`
	Sites         []SitePlan     `json:"sites"`
	PostTypes     []PostTypePlan `json:"post_types"`
	Added         []ReasonCount  `json:"added_by_dependencies"`
	Tables        []TablePlan    `json:"tables"`
	Warnings      []string       `json:"warnings"`

	Rules   map[string]Rule     `json:"-"`
	Columns map[string][]string `json:"-"`

	db *sql.DB
}

// SitePlan is the outcome for one site.
type SitePlan struct {
	BlogID    int    `json:"blog_id"`
	Prefix    string `json:"prefix"`
	Domain    string `json:"domain,omitempty"`
	Path      string `json:"path,omitempty"`
	Excluded  bool   `json:"excluded"`
	Posts     int64  `json:"posts"`
	KeptPosts int64  `json:"kept_posts"`
	Bytes     int64  `json:"bytes"` // estimated output size of the site's tables
}

// PostTypePlan is the outcome for one post type of one site.
type PostTypePlan struct {
	Site   int    `json:"site"`
	Type   string `json:"type"`
	Rule   string `json:"rule"`   // such as "latest 10 (publish)" or "always kept"
	Source string `json:"source"` // config, default, or fixed
	Total  int64  `json:"total"`
	Seeds  int64  `json:"seeds"` // picked by the rule itself
	Kept   int64  `json:"kept"`  // seeds plus posts added as dependencies
	// Bytes is the size of the kept posts and all their post meta.
	Bytes int64 `json:"bytes"`
}

// ReasonCount is how many posts of one site one kind of reference added.
type ReasonCount struct {
	Site   int    `json:"site"`
	Reason string `json:"reason"`
	Posts  int64  `json:"posts"`
}

// TablePlan is the outcome for one table.
type TablePlan struct {
	Name     string `json:"name"`
	Site     int    `json:"site,omitempty"`
	Role     string `json:"role,omitempty"` // core table role, such as postmeta
	Rule     Rule   `json:"rule"`
	Rows     int64  `json:"rows"`
	KeptRows int64  `json:"kept_rows"`
	// Bytes is the estimated output size: kept row bytes plus the schema.
	Bytes int64 `json:"bytes"`
	// Approximate is set when the index cannot give an exact row count.
	Approximate bool `json:"approximate,omitempty"`
}

// Built-in lists from SPEC.md.
var (
	// followedMetaKeys are post meta keys whose values hold post IDs.
	followedMetaKeys = []string{
		"_thumbnail_id",
		"_menu_item_object_id",
		"_product_image_gallery",
		"_upsell_ids",
		"_crosssell_ids",
		"_children",
		"_yoast_wpseo_opengraph-image-id",
		"_yoast_wpseo_twitter-image-id",
	}

	// acfPostFieldTypes are ACF field types whose values are post IDs.
	acfPostFieldTypes = []string{"image", "file", "gallery", "relationship", "post_object", "page_link"}

	// childTypes are post types kept whenever their parent is kept.
	childTypes = []string{"product_variation"}

	// optionPostRefs are options that hold one post ID.
	optionPostRefs = []string{
		"page_on_front", "page_for_posts", "wp_page_for_privacy_policy",
		"woocommerce_shop_page_id", "woocommerce_cart_page_id", "woocommerce_checkout_page_id",
		"woocommerce_myaccount_page_id", "woocommerce_terms_page_id",
	}
)

// schemaOverheadBytes approximates the statements around each table in a
// dump: DROP TABLE, SET, LOCK TABLES, and comments.
const schemaOverheadBytes = 400

// Build makes a plan from an index and a config. Close the plan when done.
func Build(ctx context.Context, indexPath string, cfg *config.Config) (*Plan, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	// One connection, so every statement sees the attached index and the
	// keep tables.
	db.SetMaxOpenConns(1)
	p := &Plan{db: db, TargetBytes: int64(cfg.TargetSizeMB * (1 << 20)), Warnings: []string{}}
	b := &builder{ctx: ctx, db: db, cfg: cfg, p: p}
	if err := b.run(indexPath); err != nil {
		db.Close()
		return nil, err
	}
	return p, nil
}

// Close releases the plan's database.
func (p *Plan) Close() error { return p.db.Close() }

// DB returns the database holding the keep tables, for pass 2.
func (p *Plan) DB() *sql.DB { return p.db }

type site struct {
	id     int
	prefix string
	domain string
	path   string
	cfg    *config.Site
}

type builder struct {
	ctx context.Context
	db  *sql.DB
	cfg *config.Config
	p   *Plan

	sites    []site // every site, in blog ID order
	included []site
	excluded map[int]bool

	err error
}

// warnf adds a warning. Pass site 0 for warnings about the whole dump.
func (b *builder) warnf(siteID int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if siteID != 0 && len(b.sites) > 1 {
		msg = fmt.Sprintf("site %d: %s", siteID, msg)
	}
	b.p.Warnings = append(b.p.Warnings, msg)
}

// exec runs a statement unless an earlier one failed, and returns rows affected.
func (b *builder) exec(query string, args ...any) int64 {
	if b.err != nil {
		return 0
	}
	res, err := b.db.ExecContext(b.ctx, query, args...)
	if err != nil {
		b.err = fmt.Errorf("plan: %w\nquery: %s", err, query)
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func (b *builder) queryRows(query string, fn func(*sql.Rows) error, args ...any) {
	if b.err != nil {
		return
	}
	rows, err := b.db.QueryContext(b.ctx, query, args...)
	if err != nil {
		b.err = fmt.Errorf("plan: %w\nquery: %s", err, query)
		return
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			b.err = err
			return
		}
	}
	if err := rows.Err(); err != nil {
		b.err = err
	}
}

func (b *builder) scalar(query string, dest any, args ...any) {
	if b.err != nil {
		return
	}
	if err := b.db.QueryRowContext(b.ctx, query, args...).Scan(dest); err != nil {
		b.err = fmt.Errorf("plan: %w\nquery: %s", err, query)
	}
}

func (b *builder) run(indexPath string) error {
	b.exec(fmt.Sprintf("ATTACH %s AS ix (READ_ONLY)", sqlString(indexPath)))
	if b.err != nil {
		return fmt.Errorf("plan: open %s: %w", indexPath, b.err)
	}
	if err := index.CheckVersion(b.db, "ix"); err != nil {
		return fmt.Errorf("plan: %s: %w", indexPath, err)
	}
	b.scalar(`SELECT value FROM ix.info WHERE key = 'prefix'`, &b.p.Prefix)

	b.loadSites()
	b.seeds()
	b.dependencies()
	b.filters()
	b.tables()
	b.postTypeReport()
	b.siteReport()
	b.checks()
	return b.err
}

func (b *builder) loadSites() {
	b.excluded = map[int]bool{}
	var blogRows int64
	b.scalar(`SELECT count(*) FROM ix.blogs`, &blogRows)
	var orphans []string
	b.queryRows(`SELECT blog_id, prefix, coalesce(domain, ''), coalesce(path, ''),
			blog_id IN (SELECT blog_id FROM ix.blogs)
		FROM ix.sites ORDER BY blog_id`, func(r *sql.Rows) error {
		var s site
		var listed bool
		if err := r.Scan(&s.id, &s.prefix, &s.domain, &s.path, &listed); err != nil {
			return err
		}
		s.cfg = b.cfg.ForSite(s.id)
		// Tables of a site that wp_blogs no longer lists are left over from
		// a deleted site. WordPress cannot reach them, so they are dropped
		// unless the config says otherwise.
		if blogRows > 0 && !listed && s.id != 1 && !s.cfg.ExcludeSet {
			s.cfg.Exclude = true
			orphans = append(orphans, strconv.Itoa(s.id))
		}
		b.sites = append(b.sites, s)
		if s.cfg.Exclude {
			b.excluded[s.id] = true
		} else {
			b.included = append(b.included, s)
		}
		return nil
	})
	if len(orphans) > 0 {
		b.warnf(0, "sites %s have tables but no row in %sblogs, so they are left over from deleted sites; their tables are dropped (set sites.<id>.exclude: false to keep one)",
			strings.Join(orphans, ", "), b.p.Prefix)
	}
	b.exec(`CREATE TABLE kept_sites (blog_id INTEGER)`)
	for _, s := range b.included {
		b.exec(`INSERT INTO kept_sites VALUES (?)`, s.id)
	}
	known := map[string]bool{"*": true}
	for _, s := range b.sites {
		known[strconv.Itoa(s.id)] = true
	}
	for _, key := range sortedKeys(b.cfg.Sites) {
		if !known[key] {
			b.warnf(0, "sites.%s: no site with this blog ID is in the dump", key)
		}
	}
}

// --- step 1: seeds ---

// dropFilter is the SQL condition for posts that may never be kept.
func dropFilter(alias string) string {
	return fmt.Sprintf("%[1]s.type NOT IN (%[2]s) AND coalesce(%[1]s.status, '') <> 'auto-draft'",
		alias, sqlList(config.FixedTypes("dropped")))
}

func (b *builder) seeds() {
	b.exec(`CREATE TABLE seed (site INTEGER, id BIGINT, reason VARCHAR)`)
	b.exec(fmt.Sprintf(`INSERT INTO seed SELECT p.site, p.id, 'always kept' FROM ix.posts p
		WHERE p.site IN (SELECT blog_id FROM kept_sites) AND p.type IN (%s) AND %s`,
		sqlList(config.FixedTypes("kept")), dropFilter("p")))

	for _, s := range b.included {
		b.seedSite(s)
	}

	b.exec(fmt.Sprintf(`CREATE TABLE keep AS
		SELECT s.site, s.id, 0 AS depth, min(s.reason) AS reason
		FROM seed s JOIN ix.posts p ON p.site = s.site AND p.id = s.id
		WHERE %s GROUP BY s.site, s.id`, dropFilter("p")))
}

func (b *builder) seedSite(s site) {
	var types []string
	b.queryRows(`SELECT DISTINCT type FROM ix.posts WHERE site = ? AND type IS NOT NULL ORDER BY type`, func(r *sql.Rows) error {
		var t string
		if err := r.Scan(&t); err != nil {
			return err
		}
		types = append(types, t)
		return nil
	}, s.id)

	var defaulted []string
	for _, typ := range types {
		if _, fixed := config.FixedPostTypes[typ]; fixed {
			continue
		}
		pt, fromConfig := s.cfg.PostTypeFor(typ)
		if !fromConfig {
			if typ == "attachment" {
				// Attachments come in as dependencies of kept posts.
				continue
			}
			defaulted = append(defaulted, typ)
		}
		b.seedType(s.id, typ, pt)
	}
	if len(defaulted) > 0 {
		noun, verb := "post types are", "use"
		if len(defaulted) == 1 {
			noun, verb = "post type is", "uses"
		}
		b.warnf(s.id, "%d %s not in the config and %s default_post_type (%s): %s",
			len(defaulted), noun, verb, describeMode(s.cfg.DefaultPostType), strings.Join(defaulted, ", "))
	}
	if len(b.sites) == 1 {
		for _, name := range s.cfg.SortedPostTypes() {
			if !slices.Contains(types, name) {
				b.warnf(s.id, "post_types.%s: no posts of this type are in the dump", name)
			}
		}
	}

	// Posts named by options, such as the front page.
	values := map[string]string{}
	b.queryRows(`SELECT name, value FROM ix.options WHERE site = ? AND value IS NOT NULL`, func(r *sql.Rows) error {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			return err
		}
		values[k] = v
		return nil
	}, s.id)
	for _, name := range optionPostRefs {
		if id, err := strconv.ParseInt(strings.TrimSpace(values[name]), 10, 64); err == nil && id > 0 {
			b.exec(`INSERT INTO seed VALUES (?, ?, ?)`, s.id, id, "option:"+name)
		}
	}
	if v, err := phpser.Unserialize([]byte(values["sticky_posts"])); err == nil {
		phpser.Walk(v, func(x phpser.Value) {
			if x.Kind == phpser.Int && x.Int > 0 {
				b.exec(`INSERT INTO seed VALUES (?, ?, 'option:sticky_posts')`, s.id, x.Int)
			}
		})
	}
}

func describeMode(pt *config.PostType) string {
	switch pt.Mode {
	case config.ModeLatest:
		return fmt.Sprintf("latest %d", pt.Count)
	case config.ModePerTerm:
		return fmt.Sprintf("%d per term of %s", pt.PerTerm, strings.Join(sortedKeys(pt.Taxonomies), ", "))
	}
	return pt.Mode
}

// seedType adds the seed posts of one post type of one site.
func (b *builder) seedType(siteID int, typ string, pt *config.PostType) {
	var all []string
	for status, amount := range pt.Statuses {
		if amount.All {
			all = append(all, status)
		}
	}
	sort.Strings(all)
	reason := "type:" + typ + ":" + pt.Mode

	if len(all) > 0 {
		inStatus := sqlList(all)
		switch pt.Mode {
		case config.ModeAll:
			b.exec(fmt.Sprintf(`INSERT INTO seed SELECT site, id, ? FROM ix.posts
				WHERE site = ? AND type = ? AND status IN (%s)`, inStatus), reason, siteID, typ)
		case config.ModeLatest:
			b.exec(fmt.Sprintf(`INSERT INTO seed SELECT site, id, ? FROM ix.posts
				WHERE site = ? AND type = ? AND status IN (%s)
				ORDER BY date DESC NULLS LAST, id DESC LIMIT %d`, inStatus, pt.Count), reason, siteID, typ)
		case config.ModePerTerm:
			for _, tax := range sortedKeys(pt.Taxonomies) {
				b.seedPerTerm(siteID, typ, tax, pt.Taxonomies[tax], pt.PerTerm, inStatus, reason+":"+tax)
			}
		}
	}

	// Extra posts by status, such as a few drafts for editorial screens.
	for _, status := range sortedKeys(pt.Statuses) {
		amount := pt.Statuses[status]
		if amount.All || amount.N == 0 {
			continue
		}
		b.exec(fmt.Sprintf(`INSERT INTO seed SELECT site, id, ? FROM ix.posts
			WHERE site = ? AND type = ? AND status = ?
			ORDER BY date DESC NULLS LAST, id DESC LIMIT %d`, amount.N),
			"type:"+typ+":status:"+status, siteID, typ, status)
	}
}

// seedPerTerm adds the newest perTerm posts in each of the chosen terms.
func (b *builder) seedPerTerm(siteID int, typ, tax string, pick config.TaxonomyPick, perTerm int, inStatus, reason string) {
	order := "c.n DESC, tt.term_taxonomy_id"
	if pick.Order == "name" {
		order = "t.name, tt.term_taxonomy_id"
	}
	limit := ""
	if !pick.MaxTerms.All {
		limit = fmt.Sprintf("LIMIT %d", pick.MaxTerms.N)
	}
	b.exec(fmt.Sprintf(`INSERT INTO seed
		WITH eligible AS (
			SELECT id, date FROM ix.posts WHERE site = $1 AND type = $2 AND status IN (%[1]s)
		), rel AS (
			SELECT object_id, term_taxonomy_id FROM ix.term_relationships WHERE site = $1
		), chosen AS (
			SELECT tt.term_taxonomy_id
			FROM ix.term_taxonomy tt
			JOIN (SELECT r.term_taxonomy_id, count(*) AS n
				FROM rel r JOIN eligible e ON e.id = r.object_id
				GROUP BY 1) c USING (term_taxonomy_id)
			LEFT JOIN ix.terms t ON t.site = $1 AND t.term_id = tt.term_id
			WHERE tt.site = $1 AND tt.taxonomy = $3
			ORDER BY %[2]s %[3]s
		)
		SELECT $1, id, $4 FROM (
			SELECT e.id, row_number() OVER (PARTITION BY r.term_taxonomy_id ORDER BY e.date DESC NULLS LAST, e.id DESC) AS rn
			FROM rel r
			JOIN chosen USING (term_taxonomy_id)
			JOIN eligible e ON e.id = r.object_id
		) WHERE rn <= %[4]d`, inStatus, order, limit, perTerm), siteID, typ, tax, reason)
}

// --- step 2: dependencies ---

func (b *builder) dependencies() {
	// Meta keys whose values are followed as post references, per site.
	b.exec(`CREATE TABLE follow_keys (site INTEGER, meta_key VARCHAR, reason VARCHAR)`)
	for _, s := range b.included {
		for _, k := range append(slices.Clone(followedMetaKeys), s.cfg.ExtraMetaKeys...) {
			b.exec(`INSERT INTO follow_keys VALUES (?, ?, ?)`, s.id, k, "meta:"+k)
		}
		b.acfKeys(s.id)
	}

	inSites := `site IN (SELECT blog_id FROM kept_sites)`
	b.exec(fmt.Sprintf(`CREATE TABLE edges AS
		SELECT e.site, e.src, e.dst, e.reason, p.type = 'attachment' AS to_attachment
		FROM (
			SELECT m.site, m.post_id AS src, m.ref_id AS dst, f.reason
				FROM ix.meta_refs m JOIN follow_keys f ON f.site = m.site AND f.meta_key = m.meta_key
			UNION ALL
			SELECT site, post_id, ref_id, 'content:' || source FROM ix.content_refs WHERE %[1]s
			UNION ALL
			SELECT site, id, parent, 'parent' FROM ix.posts WHERE parent > 0 AND type <> 'attachment' AND %[1]s
			UNION ALL
			SELECT site, parent, id, 'child:' || type FROM ix.posts WHERE parent > 0 AND type IN (%[2]s) AND %[1]s
		) e
		JOIN ix.posts p ON p.site = e.site AND p.id = e.dst
		WHERE %[3]s`, inSites, sqlList(childTypes), dropFilter("p")))

	add := func(depth int, cond string) int64 {
		return b.exec(fmt.Sprintf(`INSERT INTO keep
			SELECT e.site, e.dst, %d, min(e.reason)
			FROM edges e JOIN keep k ON k.site = e.site AND k.id = e.src
			WHERE %s AND NOT EXISTS (SELECT 1 FROM keep k2 WHERE k2.site = e.site AND k2.id = e.dst)
			GROUP BY e.site, e.dst`, depth, cond))
	}
	depth := *b.cfg.DependencyDepth
	for d := 1; d <= depth; d++ {
		if add(d, fmt.Sprintf("k.depth = %d", d-1)) == 0 {
			break
		}
	}
	// Some references are followed past the depth limit, because the site
	// breaks without them: attachments, parents, menu targets, and children
	// such as product variations. These chains end quickly.
	exempt := `(e.to_attachment OR e.reason IN ('parent', 'meta:_menu_item_object_id') OR e.reason LIKE 'child:%')`
	for d := depth + 1; d <= depth+100; d++ {
		if add(d, exempt) == 0 {
			break
		}
	}

	b.p.Added = []ReasonCount{}
	b.queryRows(`SELECT site, reason, count(*) FROM keep WHERE depth > 0 GROUP BY site, reason ORDER BY site, count(*) DESC, reason`, func(r *sql.Rows) error {
		var rc ReasonCount
		if err := r.Scan(&rc.Site, &rc.Reason, &rc.Posts); err != nil {
			return err
		}
		b.p.Added = append(b.p.Added, rc)
		return nil
	})

	// A listed meta key with no ID-like value anywhere is likely a typo.
	var keys []string
	for _, s := range b.included {
		for _, k := range s.cfg.ExtraMetaKeys {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	for _, k := range keys {
		var n int64
		b.scalar(`SELECT count(*) FROM ix.meta_refs WHERE meta_key = ?`, &n, k)
		if n == 0 {
			b.warnf(0, "references.extra_meta_keys: no value of meta key %q looks like a post ID", k)
		}
	}
}

// acfKeys adds the meta keys of ACF fields that hold post IDs. A top-level
// field's meta key is its name. A field inside a repeater, group, or
// flexible content field has a longer key that ends in _<name>.
func (b *builder) acfKeys(siteID int) {
	type field struct {
		name     string
		subfield bool
	}
	var fields []field
	b.queryRows(fmt.Sprintf(`SELECT f.field_name, f.parent IN (SELECT post_id FROM ix.acf_fields WHERE site = $1)
		FROM ix.acf_fields f WHERE f.site = $1 AND f.field_type IN (%s) AND f.field_name <> ''`, sqlList(acfPostFieldTypes)),
		func(r *sql.Rows) error {
			var f field
			if err := r.Scan(&f.name, &f.subfield); err != nil {
				return err
			}
			fields = append(fields, f)
			return nil
		}, siteID)
	if len(fields) == 0 {
		return
	}
	var keys []string
	b.queryRows(`SELECT DISTINCT meta_key FROM ix.meta_refs WHERE site = ? AND meta_key IS NOT NULL AND meta_key NOT LIKE '\_%' ESCAPE '\'`,
		func(r *sql.Rows) error {
			var k string
			if err := r.Scan(&k); err != nil {
				return err
			}
			keys = append(keys, k)
			return nil
		}, siteID)
	for _, k := range keys {
		for _, f := range fields {
			if k == f.name || (f.subfield && strings.HasSuffix(k, "_"+f.name)) {
				b.exec(`INSERT INTO follow_keys VALUES (?, ?, ?)`, siteID, k, "acf:"+k)
				break
			}
		}
	}
}

// --- step 3: other tables ---

func (b *builder) filters() {
	// Every post meta row of every kept post. Savings come from keeping
	// fewer posts, not from trimming their meta. meta.exclude_keys is an
	// explicit opt-out.
	where := ""
	var args []any
	if len(b.cfg.Meta.ExcludeKeys) > 0 {
		var conds []string
		for _, k := range b.cfg.Meta.ExcludeKeys {
			conds = append(conds, `coalesce(m.meta_key, '') LIKE ? ESCAPE '\'`)
			args = append(args, globToLike(k))
		}
		where = "WHERE NOT (" + strings.Join(conds, " OR ") + ")"
	}
	b.exec(fmt.Sprintf(`CREATE TABLE keep_postmeta AS
		SELECT m.site, m.meta_id FROM ix.postmeta m JOIN keep k ON k.site = m.site AND k.id = m.post_id
		%s`, where), args...)

	// Terms: all of them, except that a big flat taxonomy keeps only the
	// terms that kept posts use. Ancestors of kept terms are always kept.
	b.exec(`CREATE TABLE keep_tt AS
		WITH RECURSIVE big_flat AS (
			SELECT site, taxonomy FROM ix.term_taxonomy GROUP BY site, taxonomy
			HAVING count(*) > ? AND NOT bool_or(parent > 0)
		), used AS (
			SELECT DISTINCT tr.site, tr.term_taxonomy_id FROM ix.term_relationships tr
			JOIN keep k ON k.site = tr.site AND k.id = tr.object_id
		), base AS (
			SELECT tt.site, tt.term_taxonomy_id, tt.term_id, tt.taxonomy, tt.parent FROM ix.term_taxonomy tt
			WHERE tt.site IN (SELECT blog_id FROM kept_sites)
				AND (NOT EXISTS (SELECT 1 FROM big_flat b WHERE b.site = tt.site AND b.taxonomy = tt.taxonomy)
					OR EXISTS (SELECT 1 FROM used u WHERE u.site = tt.site AND u.term_taxonomy_id = tt.term_taxonomy_id))
		), anc AS (
			SELECT site, term_taxonomy_id, term_id, taxonomy, parent FROM base
			UNION
			SELECT tt.site, tt.term_taxonomy_id, tt.term_id, tt.taxonomy, tt.parent
			FROM ix.term_taxonomy tt JOIN anc a ON tt.site = a.site AND tt.term_id = a.parent AND tt.taxonomy = a.taxonomy
			WHERE a.parent > 0
		)
		SELECT DISTINCT site, term_taxonomy_id, term_id, taxonomy FROM anc`, *b.cfg.Taxonomies.PruneUnusedFlatTermsOver)
	b.exec(`CREATE TABLE keep_terms AS SELECT DISTINCT site, term_id FROM keep_tt`)
	b.exec(`CREATE TABLE keep_termmeta AS
		SELECT tm.site, tm.meta_id FROM ix.termmeta tm JOIN keep_terms kt ON kt.site = tm.site AND kt.term_id = tm.term_id`)
	// Link categories point at links, which are all kept.
	b.exec(`CREATE TABLE keep_tr AS
		SELECT tr.site, tr.object_id, tr.term_taxonomy_id FROM ix.term_relationships tr
		JOIN keep_tt tt ON tt.site = tr.site AND tt.term_taxonomy_id = tr.term_taxonomy_id
		WHERE EXISTS (SELECT 1 FROM keep k WHERE k.site = tr.site AND k.id = tr.object_id) OR tt.taxonomy = 'link_category'`)

	// The newest approved comments of each kept post, plus their parents.
	b.exec(`CREATE TABLE keep_comments AS
		WITH RECURSIVE base AS (
			SELECT site, comment_id FROM (
				SELECT c.site, c.comment_id,
					row_number() OVER (PARTITION BY c.site, c.post_id ORDER BY c.date DESC NULLS LAST, c.comment_id DESC) AS rn
				FROM ix.comments c JOIN keep k ON k.site = c.site AND k.id = c.post_id
				WHERE c.approved = '1'
			) WHERE rn <= ?
		), anc AS (
			SELECT site, comment_id FROM base
			UNION
			SELECT c.site, c.parent FROM ix.comments c JOIN anc a ON c.site = a.site AND c.comment_id = a.comment_id
			WHERE c.parent > 0
		)
		SELECT DISTINCT site, comment_id FROM anc`, *b.cfg.Comments.PerPost)
	b.exec(`CREATE TABLE keep_commentmeta AS
		SELECT cm.site, cm.meta_id FROM ix.commentmeta cm JOIN keep_comments kc ON kc.site = cm.site AND kc.comment_id = cm.comment_id`)

	// Users are shared by all sites: authors of kept posts and comments on
	// any kept site, plus users with the listed roles on a kept site.
	b.exec(fmt.Sprintf(`CREATE TABLE keep_users AS
		SELECT DISTINCT id FROM ix.users WHERE
			id IN (SELECT p.author FROM ix.posts p JOIN keep k ON k.site = p.site AND k.id = p.id)
			OR id IN (SELECT c.user_id FROM ix.comments c JOIN keep_comments k ON k.site = c.site AND k.comment_id = c.comment_id WHERE c.user_id > 0)
			OR id IN (SELECT user_id FROM ix.user_roles WHERE site IN (SELECT blog_id FROM kept_sites) AND role IN (%s))`,
		sqlList(b.cfg.Users.IncludeRoles)))
	// User meta of kept users, without the per-site keys of excluded sites,
	// such as wp_7_capabilities.
	var drop []string
	var dropArgs []any
	for _, s := range b.sites {
		if b.excluded[s.id] {
			drop = append(drop, `meta_key LIKE ? ESCAPE '\'`)
			dropArgs = append(dropArgs, globToLike(s.prefix+"*"))
		}
	}
	userWhere := ""
	if len(drop) > 0 {
		userWhere = "AND NOT (" + strings.Join(drop, " OR ") + ")"
	}
	b.exec(fmt.Sprintf(`CREATE TABLE keep_usermeta AS
		SELECT umeta_id FROM ix.usermeta WHERE user_id IN (SELECT id FROM keep_users) %s`, userWhere), dropArgs...)

	// Every option of a kept site except transients.
	b.exec(`CREATE TABLE keep_options AS SELECT site, option_id FROM ix.options
		WHERE site IN (SELECT blog_id FROM kept_sites)
			AND NOT (name LIKE '\_transient\_%' ESCAPE '\' OR name LIKE '\_site\_transient\_%' ESCAPE '\')`)
}

// coreEstimates gives the SQL for the kept row count and bytes of each core
// table role. Site tables take the blog ID as $1.
var coreEstimates = map[string]string{
	"posts":              `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.posts x JOIN keep k ON k.site = x.site AND k.id = x.id WHERE x.site = $1`,
	"postmeta":           `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.postmeta x JOIN keep_postmeta k ON k.site = x.site AND k.meta_id = x.meta_id WHERE x.site = $1`,
	"terms":              `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.terms x JOIN keep_terms k ON k.site = x.site AND k.term_id = x.term_id WHERE x.site = $1`,
	"term_taxonomy":      `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.term_taxonomy x JOIN keep_tt k ON k.site = x.site AND k.term_taxonomy_id = x.term_taxonomy_id WHERE x.site = $1`,
	"term_relationships": `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.term_relationships x JOIN keep_tr k ON k.site = x.site AND k.object_id = x.object_id AND k.term_taxonomy_id = x.term_taxonomy_id WHERE x.site = $1`,
	"termmeta":           `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.termmeta x JOIN keep_termmeta k ON k.site = x.site AND k.meta_id = x.meta_id WHERE x.site = $1`,
	"comments":           `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.comments x JOIN keep_comments k ON k.site = x.site AND k.comment_id = x.comment_id WHERE x.site = $1`,
	"commentmeta":        `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.commentmeta x JOIN keep_commentmeta k ON k.site = x.site AND k.meta_id = x.meta_id WHERE x.site = $1`,
	"options":            `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.options x JOIN keep_options k ON k.site = x.site AND k.option_id = x.option_id WHERE x.site = $1`,
	"users":              `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.users x JOIN keep_users USING (id)`,
	"usermeta":           `SELECT count(*), coalesce(sum(x.bytes), 0) FROM ix.usermeta x JOIN keep_usermeta USING (umeta_id)`,
}

func (b *builder) tables() {
	type row struct {
		info      tableInfo
		rows      int64
		createLen int64
	}
	var rows []row
	b.queryRows(`SELECT t.name, coalesce(t.site, 0), t.role, t.rows, t.row_bytes, t.columns, length(t.create_sql),
			(SELECT any_value(column_name) FROM ix.object_rows o WHERE o.tbl = t.name)
		FROM ix.tables t ORDER BY t.ord`, func(r *sql.Rows) error {
		var x row
		var cols string
		var objectCol sql.NullString
		if err := r.Scan(&x.info.name, &x.info.site, &x.info.role, &x.rows, &x.info.rowBytes, &cols, &x.createLen, &objectCol); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(cols), &x.info.columns); err != nil {
			return err
		}
		x.info.objectCol = objectCol.String
		rows = append(rows, x)
		return nil
	})
	if b.err != nil {
		return
	}

	sitePrefix := map[int]string{}
	for _, s := range b.sites {
		sitePrefix[s.id] = s.prefix
	}
	res := newResolver(b.cfg, b.p.Prefix, sitePrefix, b.excluded)
	b.p.Rules = map[string]Rule{}
	b.p.Columns = map[string][]string{}

	for _, x := range rows {
		rule, err := res.resolve(x.info)
		if err != nil {
			b.err = fmt.Errorf("config: %w", err)
			return
		}
		tp := TablePlan{Name: x.info.name, Site: x.info.site, Role: x.info.role, Rule: rule, Rows: x.rows}
		switch rule.Action {
		case ActionCore:
			if q, ok := coreEstimates[x.info.role]; ok {
				var args []any
				if x.info.site != 0 {
					args = append(args, x.info.site)
				}
				if b.err == nil {
					if err := b.db.QueryRowContext(b.ctx, q, args...).Scan(&tp.KeptRows, &tp.Bytes); err != nil {
						b.err = fmt.Errorf("plan: estimate %s: %w", x.info.name, err)
						return
					}
				}
			} else {
				// links: kept whole.
				tp.KeptRows, tp.Bytes = x.rows, x.info.rowBytes
			}
		case config.TableKeep:
			tp.KeptRows, tp.Bytes = x.rows, x.info.rowBytes
		case config.TableFilter:
			switch {
			case rule.IDs == "sites" && x.info.role == "blogs":
				b.scalar(`SELECT count(*) FROM ix.blogs WHERE blog_id IN (SELECT blog_id FROM kept_sites)`, &tp.KeptRows)
				if x.rows > 0 {
					tp.Bytes = x.info.rowBytes * tp.KeptRows / x.rows
				}
			case rule.IDs == "" && x.info.objectCol == rule.FilterBy:
				b.scalar(`SELECT count(*) FROM ix.object_rows o JOIN keep k ON k.site = o.site AND k.id = o.object_id WHERE o.tbl = ?`, &tp.KeptRows, x.info.name)
				b.scalar(`SELECT coalesce(sum(o.bytes), 0) FROM ix.object_rows o JOIN keep k ON k.site = o.site AND k.id = o.object_id WHERE o.tbl = ?`, &tp.Bytes, x.info.name)
			default:
				tp.KeptRows, tp.Bytes, tp.Approximate = x.rows, x.info.rowBytes, true
				if rule.IDs == "" {
					b.warnf(0, "table %s: pass 1 did not index column %s, so the estimate counts the whole table", x.info.name, rule.FilterBy)
				}
			}
		case config.TableEmpty:
			if rule.Source == "default" && x.rows > 0 {
				b.warnf(0, "table %s: unknown table of %s emptied; set a rule under tables: to keep or filter it", x.info.name, formatBytes(x.info.rowBytes))
			}
		}
		if x.createLen > 0 && rule.Action != ActionDrop {
			tp.Bytes += x.createLen + schemaOverheadBytes
		}
		b.p.EstimateBytes += tp.Bytes
		b.p.Tables = append(b.p.Tables, tp)
		b.p.Rules[x.info.name] = rule
		b.p.Columns[x.info.name] = x.info.columns
	}
	b.p.EstimateBytes += 2048 // dump header and footer

	for name := range b.cfg.Tables {
		if strings.ContainsAny(name, "*?[") {
			continue
		}
		if _, ok := b.p.Rules[name]; !ok {
			b.warnf(0, "tables.%s: no such table in the dump", name)
		}
	}
}

func (b *builder) postTypeReport() {
	b.queryRows(fmt.Sprintf(`WITH meta AS (
			SELECT x.site, x.post_id, sum(x.bytes) AS bytes
			FROM ix.postmeta x JOIN keep_postmeta k ON k.site = x.site AND k.meta_id = x.meta_id
			GROUP BY 1, 2
		)
		SELECT p.site, p.type, count(*), count(k.id) FILTER (WHERE k.depth = 0), count(k.id),
			coalesce(sum(p.bytes + coalesce(m.bytes, 0)) FILTER (WHERE k.id IS NOT NULL), 0) AS kept_bytes,
			p.type IN (%s), p.type IN (%s)
		FROM ix.posts p
		LEFT JOIN keep k ON k.site = p.site AND k.id = p.id
		LEFT JOIN meta m ON m.site = p.site AND m.post_id = p.id
		WHERE p.type IS NOT NULL AND p.site IN (SELECT blog_id FROM kept_sites)
		GROUP BY p.site, p.type ORDER BY p.site, kept_bytes DESC, p.type`,
		sqlList(config.FixedTypes("kept")), sqlList(config.FixedTypes("dropped"))),
		func(r *sql.Rows) error {
			var pt PostTypePlan
			var alwaysKept, alwaysDropped bool
			if err := r.Scan(&pt.Site, &pt.Type, &pt.Total, &pt.Seeds, &pt.Kept, &pt.Bytes, &alwaysKept, &alwaysDropped); err != nil {
				return err
			}
			switch {
			case alwaysKept:
				pt.Rule, pt.Source = "always kept", "fixed"
			case alwaysDropped:
				pt.Rule, pt.Source = "always dropped", "fixed"
			default:
				c, fromConfig := b.cfg.ForSite(pt.Site).PostTypeFor(pt.Type)
				pt.Source = "config"
				if !fromConfig {
					pt.Source = "default"
					if pt.Type == "attachment" {
						c = &config.PostType{Mode: config.ModeNone}
						pt.Source = "fixed"
					}
				}
				pt.Rule = describeMode(c)
				if c.Mode != config.ModeNone {
					pt.Rule += " (" + describeStatuses(c.Statuses) + ")"
				}
			}
			b.p.PostTypes = append(b.p.PostTypes, pt)
			return nil
		})
}

func (b *builder) siteReport() {
	bytes := map[int]int64{}
	for _, t := range b.p.Tables {
		bytes[t.Site] += t.Bytes
	}
	for _, s := range b.sites {
		sp := SitePlan{BlogID: s.id, Prefix: s.prefix, Domain: s.domain, Path: s.path, Excluded: b.excluded[s.id], Bytes: bytes[s.id]}
		b.scalar(`SELECT count(*) FROM ix.posts WHERE site = ?`, &sp.Posts, s.id)
		b.scalar(`SELECT count(*) FROM keep WHERE site = ?`, &sp.KeptPosts, s.id)
		b.p.Sites = append(b.p.Sites, sp)
	}
}

func describeStatuses(s map[string]config.Amount) string {
	var parts []string
	for _, k := range sortedKeys(s) {
		if s[k].All {
			parts = append(parts, k)
		} else if s[k].N > 0 {
			parts = append(parts, fmt.Sprintf("%s +%d", k, s[k].N))
		}
	}
	return strings.Join(parts, ", ")
}

// checks adds warnings that SPEC.md asks for.
func (b *builder) checks() {
	for _, s := range b.included {
		var ppp sql.NullString
		b.scalar(`SELECT any_value(value) FROM ix.options WHERE site = ? AND name = 'posts_per_page'`, &ppp, s.id)
		n, err := strconv.Atoi(ppp.String)
		if err != nil {
			continue
		}
		for _, name := range s.cfg.SortedPostTypes() {
			pt := s.cfg.PostTypes[name]
			if pt.Mode == config.ModePerTerm && pt.PerTerm <= n {
				b.warnf(s.id, "post_types.%s.per_term is %d, not more than posts_per_page (%d), so archive pagination cannot be tested", name, pt.PerTerm, n)
			}
		}
	}
	if b.p.EstimateBytes > b.p.TargetBytes {
		top := slices.Clone(b.p.Tables)
		slices.SortFunc(top, func(x, y TablePlan) int { return int(y.Bytes - x.Bytes) })
		var parts []string
		for _, t := range top[:min(3, len(top))] {
			parts = append(parts, fmt.Sprintf("%s %s", t.Name, formatBytes(t.Bytes)))
		}
		b.warnf(0, "estimate %s is over the target %s; biggest tables: %s. Keep fewer posts or post types to shrink it",
			formatBytes(b.p.EstimateBytes), formatBytes(b.p.TargetBytes), strings.Join(parts, ", "))
	}
}

// --- helpers ---

// sqlString quotes s as an SQL string literal.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sqlList quotes strings for an IN list. An empty list matches nothing.
func sqlList(items []string) string {
	if len(items) == 0 {
		return "NULL"
	}
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = sqlString(s)
	}
	return strings.Join(q, ", ")
}

// globToLike turns a glob with * into a LIKE pattern with \ as escape.
func globToLike(glob string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`)
	return r.Replace(glob)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// FormatBytes formats a size for people.
func FormatBytes(n int64) string { return formatBytes(n) }
