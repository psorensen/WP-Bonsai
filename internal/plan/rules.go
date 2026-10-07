package plan

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/config"
)

// Rule is what happens to the rows of one table.
type Rule struct {
	Action   string `json:"action"`              // core, keep, empty, filter, or drop
	FilterBy string `json:"filter_by,omitempty"` // ID column, for filter
	// IDs names the ID set FilterBy is checked against: posts (the default)
	// or sites.
	IDs    string `json:"ids,omitempty"`
	Source string `json:"source"` // core, config, library:<plugin>, multisite, sites, or default
	Note   string `json:"note,omitempty"`
}

// Rule actions beyond the config ones.
const (
	// ActionCore marks a WordPress core table, filtered by the keep set.
	ActionCore = "core"
	// ActionDrop leaves the table out of the output entirely, schema
	// included. It applies to the tables of excluded sites.
	ActionDrop = "drop"
)

// unknownTableKeepBytes is the size below which an unknown table is kept
// whole. See SPEC.md, "Plugin and unknown table rules".
const unknownTableKeepBytes = 1 << 20

type libraryRule struct {
	pattern string // glob on the table name without its prefix
	action  string
	plugin  string
	note    string
}

// library holds the default rules for common plugin tables. SPEC.md lists
// the categories. A project config can override any of them.
var library = []libraryRule{
	// Rebuildable indexes.
	{"yoast_indexable", config.TableEmpty, "yoast", "rebuild after import: wp yoast index"},
	{"yoast_indexable_hierarchy", config.TableEmpty, "yoast", "rebuild after import: wp yoast index"},
	{"yoast_seo_links", config.TableEmpty, "yoast", "rebuild after import: wp yoast index"},
	{"yoast_seo_meta", config.TableEmpty, "yoast", "rebuild after import: wp yoast index"},
	{"yoast_primary_term", config.TableEmpty, "yoast", "rebuild after import: wp yoast index"},
	{"yoast_migrations", config.TableKeep, "yoast", "migration state; Yoast reruns migrations without it"},
	{"wc_product_meta_lookup", config.TableEmpty, "woocommerce", "rebuild after import: wp wc tool run regenerate_product_lookup_tables"},
	{"wc_product_attributes_lookup", config.TableEmpty, "woocommerce", "rebuild after import"},
	{"wc_category_lookup", config.TableEmpty, "woocommerce", "rebuild after import"},
	{"wc_reserved_stock", config.TableEmpty, "woocommerce", ""},

	// Logs, queues, caches.
	{"actionscheduler_*", config.TableEmpty, "action-scheduler", ""},
	{"redirection_logs", config.TableEmpty, "redirection", ""},
	{"redirection_404", config.TableEmpty, "redirection", ""},
	{"wf*", config.TableEmpty, "wordfence", ""},
	{"wfls_*", config.TableEmpty, "wordfence", ""},
	{"stream", config.TableEmpty, "stream", ""},
	{"stream_meta", config.TableEmpty, "stream", ""},
	{"wc_admin_notes", config.TableEmpty, "woocommerce", ""},
	{"wc_admin_note_actions", config.TableEmpty, "woocommerce", ""},
	{"wc_rate_limits", config.TableEmpty, "woocommerce", ""},
	{"wc_download_log", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_sessions", config.TableEmpty, "woocommerce", "personal data"},
	{"gf_form_view", config.TableEmpty, "gravityforms", "IP addresses"},

	// Personal data.
	{"wc_orders", config.TableEmpty, "woocommerce", "personal data"},
	{"wc_orders_meta", config.TableEmpty, "woocommerce", "personal data"},
	{"wc_order_*", config.TableEmpty, "woocommerce", "personal data"},
	{"wc_customer_lookup", config.TableEmpty, "woocommerce", "personal data"},
	{"wc_webhooks", config.TableEmpty, "woocommerce", ""},
	{"woocommerce_order_items", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_order_itemmeta", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_downloadable_product_permissions", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_payment_tokens", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_payment_tokenmeta", config.TableEmpty, "woocommerce", "personal data"},
	{"woocommerce_api_keys", config.TableEmpty, "woocommerce", "secrets"},
	{"gf_entry", config.TableEmpty, "gravityforms", "personal data"},
	{"gf_entry_meta", config.TableEmpty, "gravityforms", "personal data"},
	{"gf_entry_notes", config.TableEmpty, "gravityforms", "personal data"},
	{"gf_draft_submissions", config.TableEmpty, "gravityforms", "personal data"},
	{"rg_lead*", config.TableEmpty, "gravityforms", "personal data"},
	// Gravity Forms tables from before version 2.3, named rg_ instead of gf_.
	{"rg_incomplete_submissions", config.TableEmpty, "gravityforms", "personal data"},
	{"rg_form_view", config.TableEmpty, "gravityforms", "IP addresses"},
	{"rg_form", config.TableKeep, "gravityforms", ""},
	{"rg_form_meta", config.TableKeep, "gravityforms", ""},
	{"mailpoet_subscriber*", config.TableEmpty, "mailpoet", "personal data"},
	{"newsletter", config.TableEmpty, "newsletter", "personal data"},
	{"newsletter_*", config.TableEmpty, "newsletter", "personal data"},

	// Configuration.
	{"gf_form", config.TableKeep, "gravityforms", ""},
	{"gf_form_meta", config.TableKeep, "gravityforms", ""},
	{"gf_form_revisions", config.TableKeep, "gravityforms", ""},
	{"redirection_items", config.TableKeep, "redirection", ""},
	{"redirection_groups", config.TableKeep, "redirection", ""},
	{"woocommerce_attribute_taxonomies", config.TableKeep, "woocommerce", ""},
	{"woocommerce_tax_rates", config.TableKeep, "woocommerce", ""},
	{"woocommerce_tax_rate_locations", config.TableKeep, "woocommerce", ""},
	{"woocommerce_shipping_*", config.TableKeep, "woocommerce", ""},

	// Translation maps. SPEC.md asks to filter these by kept post and term
	// IDs; that needs element-type rules that do not exist yet.
	{"icl_translations", config.TableKeep, "wpml", "kept whole for now; filtering by element type is not built yet"},
	{"icl_*", config.TableKeep, "wpml", ""},
}

// tableInfo is what the rules need to know about a table.
type tableInfo struct {
	name      string
	role      string
	site      int // blog ID, or 0 for network tables and tables of no site
	rowBytes  int64
	columns   []string
	objectCol string // column indexed in object_rows, if any
}

// resolver picks a rule for each table.
type resolver struct {
	cfg        *config.Config
	prefix     string
	sitePrefix map[int]string
	excluded   map[int]bool
	globs      []string // config table keys that contain wildcards
}

func newResolver(cfg *config.Config, prefix string, sitePrefix map[int]string, excluded map[int]bool) *resolver {
	r := &resolver{cfg: cfg, prefix: prefix, sitePrefix: sitePrefix, excluded: excluded}
	for k := range cfg.Tables {
		if strings.ContainsAny(k, "*?[") {
			r.globs = append(r.globs, k)
		}
	}
	sort.Strings(r.globs)
	return r
}

func (r *resolver) resolve(t tableInfo) (Rule, error) {
	if t.site != 0 && r.excluded[t.site] {
		return Rule{Action: ActionDrop, Source: "sites", Note: fmt.Sprintf("site %d is excluded", t.site)}, nil
	}
	if t.role != "" {
		if t.site != 0 && (t.role == "comments" || t.role == "commentmeta") {
			return Rule{Action: config.TableEmpty, Source: "core", Note: "comments are always removed"}, nil
		}
		if t.site != 0 {
			return Rule{Action: ActionCore, Source: "core"}, nil
		}
		switch t.role {
		case "users", "usermeta":
			return Rule{Action: ActionCore, Source: "core"}, nil
		case "blogs", "blogmeta", "blog_versions":
			if len(r.excluded) > 0 {
				return Rule{Action: config.TableFilter, FilterBy: "blog_id", IDs: "sites", Source: "multisite", Note: "rows of excluded sites removed"}, nil
			}
			return Rule{Action: config.TableKeep, Source: "multisite", Note: "network table"}, nil
		case "site", "sitemeta":
			return Rule{Action: config.TableKeep, Source: "multisite", Note: "network table"}, nil
		case "signups", "registration_log":
			return Rule{Action: config.TableEmpty, Source: "multisite", Note: "personal data: emails and IP addresses"}, nil
		}
	}
	if cr, ok := r.configRule(t.name); ok {
		if cr.Action == config.TableFilter && len(t.columns) > 0 && !slices.Contains(t.columns, cr.FilterBy) {
			return Rule{}, fmt.Errorf("tables.%s: filter_by column %q is not in the table; its columns are %s",
				t.name, cr.FilterBy, strings.Join(t.columns, ", "))
		}
		if cr.Action == config.TableFilter && t.site == 0 {
			return Rule{}, fmt.Errorf("tables.%s: filter_by needs a table that belongs to a site", t.name)
		}
		return Rule{Action: cr.Action, FilterBy: cr.FilterBy, Source: "config"}, nil
	}

	short, hasPrefix := t.name, false
	if p, ok := r.sitePrefix[t.site]; ok && t.site != 0 {
		short, hasPrefix = strings.CutPrefix(t.name, p)
	} else {
		short, hasPrefix = strings.CutPrefix(t.name, r.prefix)
	}
	if hasPrefix {
		for _, lr := range library {
			if ok, _ := path.Match(lr.pattern, short); ok {
				return Rule{Action: lr.action, Source: "library:" + lr.plugin, Note: lr.note}, nil
			}
		}
	}
	if t.site != 0 {
		for _, col := range []string{"post_id", "object_id"} {
			if t.objectCol == col || slices.Contains(t.columns, col) {
				return Rule{Action: config.TableFilter, FilterBy: col, Source: "default"}, nil
			}
		}
	}
	if t.rowBytes < unknownTableKeepBytes {
		return Rule{Action: config.TableKeep, Source: "default", Note: "unknown table under 1 MB"}, nil
	}
	return Rule{Action: config.TableEmpty, Source: "default", Note: "unknown table over 1 MB"}, nil
}

func (r *resolver) configRule(name string) (config.TableRule, bool) {
	if cr, ok := r.cfg.Tables[name]; ok {
		return cr, true
	}
	for _, g := range r.globs {
		if ok, _ := path.Match(g, name); ok {
			return r.cfg.Tables[g], true
		}
	}
	return config.TableRule{}, false
}
