package index

// schemaVersion changes whenever the index tables change, so an old index
// is rebuilt instead of misread.
const schemaVersion = "3"

// Every row table has a tbl column holding the source table name. Tables
// that belong to one site of a network also have a site column holding its
// blog ID; a single site is blog 1. During pass 1 rows from all table
// prefixes are kept. When pass 1 ends, the site column is filled in and rows
// from prefixes that are not a site of this install are deleted.
const schemaSQL = `
CREATE TABLE info (key VARCHAR PRIMARY KEY, value VARCHAR);

-- One row per site. A single-site install has one row, blog 1.
CREATE TABLE sites (
	blog_id INTEGER PRIMARY KEY, prefix VARCHAR, domain VARCHAR, path VARCHAR,
	public BOOLEAN, archived BOOLEAN, deleted BOOLEAN, spam BOOLEAN
);

-- One row per table in the dump.
CREATE TABLE tables (
	name VARCHAR PRIMARY KEY,
	site INTEGER,          -- blog ID for a site's own tables; NULL for network tables and other tables
	role VARCHAR,          -- posts, postmeta, ... for core tables of a site; '' otherwise
	ord INTEGER,           -- order of appearance in the dump
	columns VARCHAR,       -- JSON array of column names
	create_sql VARCHAR,
	rows BIGINT,
	row_bytes BIGINT,      -- sum of the row tuple sizes as written in the dump
	other_bytes BIGINT     -- schema statements and INSERT headers
);

-- wp_blogs rows, read into sites when pass 1 ends.
CREATE TABLE blogs (
	tbl VARCHAR, blog_id BIGINT, domain VARCHAR, path VARCHAR,
	public VARCHAR, archived VARCHAR, deleted VARCHAR, spam VARCHAR
);

CREATE TABLE posts (
	tbl VARCHAR, site INTEGER, id BIGINT, type VARCHAR, status VARCHAR, author BIGINT,
	parent BIGINT, date TIMESTAMP, mime VARCHAR, bytes BIGINT
);
CREATE TABLE postmeta (tbl VARCHAR, site INTEGER, meta_id BIGINT, post_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- Numbers in meta values that may be post IDs. Most are not; the keep-set
-- builder follows only chosen meta keys.
CREATE TABLE meta_refs (tbl VARCHAR, site INTEGER, meta_id BIGINT, post_id BIGINT, meta_key VARCHAR, ref_id BIGINT);

-- The target of each menu item: _menu_item_type (post_type, taxonomy,
-- custom, ...), _menu_item_object (page, category, ...), and
-- _menu_item_object_id, one row per meta key. The object ID is a post ID
-- only when the type is post_type; for taxonomy it is a term ID.
CREATE TABLE menu_meta (tbl VARCHAR, site INTEGER, post_id BIGINT, meta_key VARCHAR, value VARCHAR);

-- IDs found in post_content: block attributes, wp-image classes, gallery shortcodes.
CREATE TABLE content_refs (tbl VARCHAR, site INTEGER, post_id BIGINT, ref_id BIGINT, source VARCHAR);

-- ACF field definitions, from acf-field posts.
CREATE TABLE acf_fields (
	tbl VARCHAR, site INTEGER, post_id BIGINT, field_key VARCHAR, field_name VARCHAR, field_type VARCHAR, parent BIGINT
);

CREATE TABLE terms (tbl VARCHAR, site INTEGER, term_id BIGINT, slug VARCHAR, name VARCHAR, bytes BIGINT);
CREATE TABLE term_taxonomy (
	tbl VARCHAR, site INTEGER, term_taxonomy_id BIGINT, term_id BIGINT, taxonomy VARCHAR,
	parent BIGINT, count BIGINT, bytes BIGINT
);
CREATE TABLE term_relationships (tbl VARCHAR, site INTEGER, object_id BIGINT, term_taxonomy_id BIGINT, bytes BIGINT);
CREATE TABLE termmeta (tbl VARCHAR, site INTEGER, meta_id BIGINT, term_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- Users are shared by every site of a network. They hold PII, so the index
-- keeps IDs, roles, and sizes only. user_roles.site is the site a role
-- applies to, read from the prefix of the capabilities meta key.
CREATE TABLE users (tbl VARCHAR, id BIGINT, bytes BIGINT);
CREATE TABLE usermeta (tbl VARCHAR, umeta_id BIGINT, user_id BIGINT, meta_key VARCHAR, bytes BIGINT);
CREATE TABLE user_roles (tbl VARCHAR, site INTEGER, user_id BIGINT, meta_key VARCHAR, role VARCHAR);

CREATE TABLE comments (
	tbl VARCHAR, site INTEGER, comment_id BIGINT, post_id BIGINT, approved VARCHAR, type VARCHAR,
	parent BIGINT, user_id BIGINT, date TIMESTAMP, bytes BIGINT
);
CREATE TABLE commentmeta (tbl VARCHAR, site INTEGER, meta_id BIGINT, comment_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- value is kept only for the options named in keptOptionValues.
CREATE TABLE options (tbl VARCHAR, site INTEGER, option_id BIGINT, name VARCHAR, autoload VARCHAR, bytes BIGINT, value VARCHAR);

-- Rows of non-core tables that have a post_id or object_id column, for
-- filtering those tables by kept posts.
CREATE TABLE object_rows (tbl VARCHAR, site INTEGER, column_name VARCHAR, object_id BIGINT, bytes BIGINT);
`

// siteTables lists the index tables whose rows belong to one site, with the
// core table suffix their rows come from.
var siteTables = []struct{ table, suffix string }{
	{"posts", "posts"},
	{"postmeta", "postmeta"},
	{"meta_refs", "postmeta"},
	{"menu_meta", "postmeta"},
	{"content_refs", "posts"},
	{"acf_fields", "posts"},
	{"terms", "terms"},
	{"term_taxonomy", "term_taxonomy"},
	{"term_relationships", "term_relationships"},
	{"termmeta", "termmeta"},
	{"comments", "comments"},
	{"commentmeta", "commentmeta"},
	{"options", "options"},
}

// networkTables lists the index tables whose rows are shared by all sites,
// with their core table suffix.
var networkTables = []struct{ table, suffix string }{
	{"users", "users"},
	{"usermeta", "usermeta"},
	{"user_roles", "usermeta"},
	{"blogs", "blogs"},
}

// siteSuffixes are the core tables every site has, without a prefix.
var siteSuffixes = []string{
	"posts", "postmeta", "terms", "term_taxonomy", "term_relationships", "termmeta",
	"comments", "commentmeta", "options", "links",
}

// networkSuffixes are core tables shared by every site of a network.
var networkSuffixes = []string{
	"users", "usermeta", "blogs", "blogmeta", "blog_versions", "site", "sitemeta",
	"signups", "registration_log",
}

// coreSuffixes are all WordPress core table names without a prefix.
var coreSuffixes = append(append([]string{}, siteSuffixes...), networkSuffixes...)

// keptOptionValues are the options whose values pass 1 stores. They hold
// post IDs or settings the keep-set builder and validation need. Other option
// values may hold secrets or PII and are not stored.
var keptOptionValues = map[string]bool{
	"siteurl": true, "home": true, "show_on_front": true, "page_on_front": true,
	"page_for_posts": true, "sticky_posts": true, "posts_per_page": true,
	"template": true, "stylesheet": true, "active_plugins": true, "db_version": true,
	"wp_page_for_privacy_policy": true,
	"woocommerce_shop_page_id":   true, "woocommerce_cart_page_id": true,
	"woocommerce_checkout_page_id": true, "woocommerce_myaccount_page_id": true,
	"woocommerce_terms_page_id": true,
}
