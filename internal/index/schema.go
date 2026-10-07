package index

// schemaVersion changes whenever the index tables change, so an old index
// is rebuilt instead of misread.
const schemaVersion = "1"

// Every row table has a tbl column holding the source table name. During
// pass 1 rows from all table prefixes are kept. When pass 1 ends, rows that
// do not belong to the main site's prefix are deleted.
const schemaSQL = `
CREATE TABLE info (key VARCHAR PRIMARY KEY, value VARCHAR);

-- One row per table in the dump.
CREATE TABLE tables (
	name VARCHAR PRIMARY KEY,
	role VARCHAR,          -- posts, postmeta, ... for core tables of the main prefix; '' otherwise
	ord INTEGER,           -- order of appearance in the dump
	columns VARCHAR,       -- JSON array of column names
	create_sql VARCHAR,
	rows BIGINT,
	row_bytes BIGINT,      -- sum of the row tuple sizes as written in the dump
	other_bytes BIGINT     -- schema statements and INSERT headers
);

CREATE TABLE posts (
	tbl VARCHAR, id BIGINT, type VARCHAR, status VARCHAR, author BIGINT,
	parent BIGINT, date TIMESTAMP, mime VARCHAR, bytes BIGINT
);
CREATE TABLE postmeta (tbl VARCHAR, meta_id BIGINT, post_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- Numbers in meta values that may be post IDs. Most are not; the keep-set
-- builder joins them against posts.
CREATE TABLE meta_refs (tbl VARCHAR, meta_id BIGINT, post_id BIGINT, meta_key VARCHAR, ref_id BIGINT);

-- IDs found in post_content: block attributes, wp-image classes, gallery shortcodes.
CREATE TABLE content_refs (tbl VARCHAR, post_id BIGINT, ref_id BIGINT, source VARCHAR);

-- ACF field definitions, from acf-field posts.
CREATE TABLE acf_fields (
	tbl VARCHAR, post_id BIGINT, field_key VARCHAR, field_name VARCHAR, field_type VARCHAR, parent BIGINT
);

CREATE TABLE terms (tbl VARCHAR, term_id BIGINT, slug VARCHAR, name VARCHAR, bytes BIGINT);
CREATE TABLE term_taxonomy (
	tbl VARCHAR, term_taxonomy_id BIGINT, term_id BIGINT, taxonomy VARCHAR,
	parent BIGINT, count BIGINT, bytes BIGINT
);
CREATE TABLE term_relationships (tbl VARCHAR, object_id BIGINT, term_taxonomy_id BIGINT, bytes BIGINT);
CREATE TABLE termmeta (tbl VARCHAR, meta_id BIGINT, term_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- Users hold PII, so the index keeps IDs, roles, and sizes only.
CREATE TABLE users (tbl VARCHAR, id BIGINT, bytes BIGINT);
CREATE TABLE usermeta (tbl VARCHAR, umeta_id BIGINT, user_id BIGINT, meta_key VARCHAR, bytes BIGINT);
CREATE TABLE user_roles (tbl VARCHAR, user_id BIGINT, meta_key VARCHAR, role VARCHAR);

CREATE TABLE comments (
	tbl VARCHAR, comment_id BIGINT, post_id BIGINT, approved VARCHAR, type VARCHAR,
	parent BIGINT, user_id BIGINT, date TIMESTAMP, bytes BIGINT
);
CREATE TABLE commentmeta (tbl VARCHAR, meta_id BIGINT, comment_id BIGINT, meta_key VARCHAR, bytes BIGINT);

-- value is kept only for the options named in keptOptionValues.
CREATE TABLE options (tbl VARCHAR, option_id BIGINT, name VARCHAR, autoload VARCHAR, bytes BIGINT, value VARCHAR);

-- Rows of non-core tables that have a post_id or object_id column, for
-- filtering those tables by kept posts.
CREATE TABLE object_rows (tbl VARCHAR, column_name VARCHAR, object_id BIGINT, bytes BIGINT);
`

// rowTables lists the index tables that have a tbl column, with the core
// table suffix their rows come from.
var rowTables = []struct{ table, suffix string }{
	{"posts", "posts"},
	{"postmeta", "postmeta"},
	{"meta_refs", "postmeta"},
	{"content_refs", "posts"},
	{"acf_fields", "posts"},
	{"terms", "terms"},
	{"term_taxonomy", "term_taxonomy"},
	{"term_relationships", "term_relationships"},
	{"termmeta", "termmeta"},
	{"users", "users"},
	{"usermeta", "usermeta"},
	{"user_roles", "usermeta"},
	{"comments", "comments"},
	{"commentmeta", "commentmeta"},
	{"options", "options"},
}

// coreSuffixes are the WordPress core table names without a prefix.
var coreSuffixes = []string{
	"posts", "postmeta", "terms", "term_taxonomy", "term_relationships", "termmeta",
	"users", "usermeta", "comments", "commentmeta", "options", "links",
}

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
