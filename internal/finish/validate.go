package finish

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/config"
)

// validate runs the checks from SPEC.md, "Validation", on the scrubbed
// database. A failed query is itself a failed check.
func (f *finisher) validate() {
	for _, s := range f.sites {
		f.referenceChecks(s)
	}
	f.piiChecks()
	f.planChecks()
}

// check runs a count query and adds a check that fails or warns when the
// count is above zero.
func (f *finisher) check(name string, siteID int, status, detail, q string, tables ...string) {
	if !f.has(tables...) {
		return
	}
	n, err := f.count(q)
	if err != nil {
		f.report.add(Check{Name: name, Site: siteID, Status: Fail, Detail: err.Error()})
		return
	}
	f.report.add(Check{Name: name, Site: siteID, Status: statusIf(n > 0, status), Count: n, Detail: ifStr(n > 0, detail)})
}

func (f *finisher) referenceChecks(s site) {
	p := s.Prefix
	f.check("post meta points at existing posts", s.BlogID, Fail, "post meta rows of posts that are not in the output",
		fmt.Sprintf(`SELECT count(*) FROM %[1]spostmeta m LEFT JOIN %[1]sposts p ON p.ID = m.post_id WHERE p.ID IS NULL`, p),
		p+"postmeta", p+"posts")
	// Only taxonomies whose objects are posts: some plugins relate terms
	// to users or other objects.
	f.check("term relationships point at existing posts and terms", s.BlogID, Fail, "term relationships of posts or terms that are not in the output",
		fmt.Sprintf(`SELECT count(*) FROM %[1]sterm_relationships tr
			LEFT JOIN %[1]sterm_taxonomy tt USING (term_taxonomy_id)
			LEFT JOIN %[1]sposts p ON p.ID = tr.object_id
			WHERE tt.term_taxonomy_id IS NULL
				OR (p.ID IS NULL AND tt.taxonomy <> 'link_category'
					AND tt.taxonomy IN (SELECT DISTINCT tt2.taxonomy FROM %[1]sterm_relationships tr2
						JOIN %[1]sterm_taxonomy tt2 USING (term_taxonomy_id) JOIN %[1]sposts p2 ON p2.ID = tr2.object_id))`, p),
		p+"term_relationships", p+"term_taxonomy", p+"posts")
	f.check("comments removed", s.BlogID, Fail, "comment rows remain",
		fmt.Sprintf(`SELECT (SELECT count(*) FROM %[1]scomments) + (SELECT count(*) FROM %[1]scommentmeta)`, p),
		p+"comments", p+"commentmeta")
	if !f.has(p+"postmeta", p+"posts") {
		return
	}

	// References whose target is missing. A target that was never in the
	// dump is a warning; one that was in the dump but not kept is a failure.
	refs := []struct{ name, q string }{
		{"featured images exist", fmt.Sprintf(`SELECT DISTINCT m.meta_value FROM %[1]spostmeta m LEFT JOIN %[1]sposts p ON p.ID = m.meta_value
			WHERE m.meta_key = '_thumbnail_id' AND m.meta_value REGEXP '^[1-9][0-9]*$' AND p.ID IS NULL`, p)},
		{"menu item targets exist", fmt.Sprintf(`SELECT DISTINCT o.meta_value FROM %[1]spostmeta o
			JOIN %[1]spostmeta t ON t.post_id = o.post_id AND t.meta_key = '_menu_item_type' AND t.meta_value = 'post_type'
			LEFT JOIN %[1]sposts p ON p.ID = o.meta_value
			WHERE o.meta_key = '_menu_item_object_id' AND o.meta_value REGEXP '^[1-9][0-9]*$' AND p.ID IS NULL`, p)},
	}
	if keys := f.acfMediaKeys(s.BlogID); len(keys) > 0 {
		refs = append(refs, struct{ name, q string }{"ACF image and file fields point at existing posts", fmt.Sprintf(`SELECT DISTINCT m.meta_value FROM %[1]spostmeta m
			LEFT JOIN %[1]sposts p ON p.ID = m.meta_value
			WHERE m.meta_key IN (%[2]s) AND m.meta_value REGEXP '^[1-9][0-9]*$' AND p.ID IS NULL`, p, sqlList(keys))})
	}
	for _, r := range refs {
		out, err := f.sql(r.q)
		if err != nil {
			f.report.add(Check{Name: r.name, Site: s.BlogID, Status: Fail, Detail: err.Error()})
			continue
		}
		var ids []int64
		for _, line := range strings.Fields(out) {
			if id, err := strconv.ParseInt(line, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		inDump, neverInDump := f.classify(s.BlogID, ids)
		switch {
		case inDump > 0:
			f.report.add(Check{Name: r.name, Site: s.BlogID, Status: Fail, Count: inDump,
				Detail: fmt.Sprintf("%d targets were in the dump but not kept; %d were never in the dump", inDump, neverInDump)})
		case neverInDump > 0:
			f.report.add(Check{Name: r.name, Site: s.BlogID, Status: Warn, Count: neverInDump,
				Detail: "targets that were never in the dump"})
		default:
			f.report.add(Check{Name: r.name, Site: s.BlogID, Status: Pass})
		}
	}
}

// classify counts post IDs that were in the dump and IDs that were not.
func (f *finisher) classify(siteID int, ids []int64) (inDump, neverInDump int64) {
	if len(ids) == 0 {
		return 0, 0
	}
	n, err := planCount(f.ctx, f.p.DB(), fmt.Sprintf(`SELECT count(*) FROM ix.posts WHERE site = ? AND id IN (%s)`, sqlInts(ids)), siteID)
	if err != nil {
		return int64(len(ids)), 0
	}
	return n, int64(len(ids)) - n
}

// acfMediaKeys returns the followed meta keys of ACF image and file fields.
func (f *finisher) acfMediaKeys(siteID int) []string {
	rows, err := f.p.DB().QueryContext(f.ctx, `SELECT DISTINCT meta_key FROM follow_keys WHERE site = ? AND field_type IN ('image', 'file')`, siteID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

// piiChecks looks for email and IP addresses the scrub should have removed.
func (f *finisher) piiChecks() {
	domains := append(append([]string{}, scrubbedDomains...), f.cfg.Scrub.AllowedDomains...)
	allowedEmails := append([]string{adminEmail, "admin@example.com"}, f.cfg.Scrub.AllowedEmails...)
	notAllowed := func(col string) string {
		return fmt.Sprintf(`%[1]s LIKE '%%@%%' AND lower(substring_index(%[1]s, '@', -1)) NOT IN (%[2]s) AND lower(%[1]s) NOT IN (%[3]s)`,
			col, sqlList(lower(domains)), sqlList(lower(allowedEmails)))
	}

	// Every column whose name says it holds an email or IP address.
	cols, err := f.sql(`SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'wordpress' AND data_type IN ('char', 'varchar', 'tinytext', 'text', 'mediumtext', 'longtext')
			AND (column_name LIKE '%email%' OR column_name REGEXP '(^|_)ip($|_)|ip_?address')`)
	if err != nil {
		f.report.add(Check{Name: "no email or IP addresses outside the allowed patterns", Status: Fail, Detail: err.Error()})
		return
	}
	var emailQ, ipQ []part
	for _, line := range strings.Split(cols, "\n") {
		table, col, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		c := "`" + col + "`"
		if strings.Contains(strings.ToLower(col), "email") {
			emailQ = append(emailQ, part{table + "." + col, fmt.Sprintf("SELECT count(*) FROM `%s` WHERE %s", table, notAllowed(c))})
		} else {
			ipQ = append(ipQ, part{table + "." + col, fmt.Sprintf(`SELECT count(*) FROM `+"`%s`"+` WHERE %[2]s REGEXP '^[0-9]{1,3}(\\.[0-9]{1,3}){3}$|:'
				AND %[2]s NOT IN ('127.0.0.1', '::1', '0.0.0.0')
				AND %[2]s NOT LIKE '192.0.2.%%' AND %[2]s NOT LIKE '198.51.100.%%' AND %[2]s NOT LIKE '203.0.113.%%'`, table, c)})
		}
	}
	// Admin email options of every site and of the network.
	for _, s := range f.sites {
		if !f.has(s.Prefix + "options") {
			continue
		}
		emailQ = append(emailQ, part{s.Prefix + "options.admin_email", fmt.Sprintf("SELECT count(*) FROM %soptions WHERE option_name IN ('admin_email', 'new_admin_email') AND %s", s.Prefix, notAllowed("option_value"))})
	}
	if f.multisite {
		emailQ = append(emailQ, part{f.prefix + "sitemeta.admin_email", fmt.Sprintf("SELECT count(*) FROM %ssitemeta WHERE meta_key IN ('admin_email', 'new_admin_email') AND %s", f.prefix, notAllowed("meta_value"))})
	}
	f.breakdown("no email addresses outside the allowed patterns", Fail,
		"email addresses that were not scrubbed; empty the table under tables:, or allow staff domains under scrub.allowed_domains", emailQ)
	f.breakdown("no IP addresses outside the allowed patterns", Fail,
		"IP addresses that were not scrubbed; empty the table under tables:", ipQ)

	// Post content is public, so addresses in it are reported, not failed.
	var contentQ []part
	for _, s := range f.sites {
		if !f.has(s.Prefix + "posts") {
			continue
		}
		// LIKE first, and bounded repeats: an unbounded pattern backtracks
		// quadratically over long runs such as base64 images in content.
		contentQ = append(contentQ, part{s.Prefix + "posts.post_content", fmt.Sprintf(`SELECT count(*) FROM %sposts WHERE post_content LIKE '%%@%%'
			AND post_content REGEXP '[A-Za-z0-9._%%+-]{1,64}@[A-Za-z0-9-]{1,63}(\\.[A-Za-z0-9-]{1,63}){0,8}\\.[A-Za-z]{2,24}'`, s.Prefix)})
	}
	f.breakdown("email addresses in post content", Warn,
		"posts whose content mentions an email address; post content is public and is not changed", contentQ)
}

// part is one counted place, such as a table column.
type part struct {
	label string
	query string // returns one count
}

// breakdown runs one count per part and adds a check whose detail names
// every part with a nonzero count. It reports counts only, never values.
func (f *finisher) breakdown(name, status, detail string, parts []part) {
	var total int64
	var hits []string
	for _, p := range parts {
		n, err := f.count(p.query)
		if err != nil {
			f.report.add(Check{Name: name, Status: Fail, Detail: p.label + ": " + err.Error()})
			return
		}
		if n > 0 {
			total += n
			hits = append(hits, fmt.Sprintf("%s %d", p.label, n))
		}
	}
	c := Check{Name: name, Status: statusIf(total > 0, status), Count: total}
	if total > 0 {
		c.Detail = detail + ". Found in: " + strings.Join(hits, ", ")
	}
	f.report.add(c)
}

// planChecks are the checks that need the index: per-term archives,
// references never in the dump, and authors never in the dump.
func (f *finisher) planChecks() {
	db := f.p.DB()
	for _, s := range f.sites {
		ppp, err := planCount(f.ctx, db, `SELECT coalesce(max(TRY_CAST(value AS BIGINT)), 0) FROM ix.options WHERE site = ? AND name = 'posts_per_page'`, s.BlogID)
		if err != nil || ppp == 0 {
			continue
		}
		site := f.cfg.ForSite(s.BlogID)
		var short int64
		for _, typ := range site.SortedPostTypes() {
			pt := site.PostTypes[typ]
			if pt.Mode != config.ModePerTerm {
				continue
			}
			for tax := range pt.Taxonomies {
				n, err := planCount(f.ctx, db, `WITH src AS (
						SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
						JOIN ix.posts p ON p.site = tr.site AND p.id = tr.object_id
						WHERE tr.site = $1 AND p.type = $2 AND p.status = 'publish' GROUP BY 1
					), kept AS (
						SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
						JOIN ix.posts p ON p.site = tr.site AND p.id = tr.object_id
						JOIN keep k ON k.site = p.site AND k.id = p.id
						WHERE tr.site = $1 AND p.type = $2 AND p.status = 'publish' GROUP BY 1
					)
					SELECT count(*) FROM src
					JOIN ix.term_taxonomy tt ON tt.site = $1 AND tt.term_taxonomy_id = src.term_taxonomy_id
					LEFT JOIN kept USING (term_taxonomy_id)
					WHERE tt.taxonomy = $3 AND src.n > $4 AND coalesce(kept.n, 0) <= $4`, s.BlogID, typ, tax, ppp)
				if err == nil {
					short += n
				}
			}
		}
		f.report.add(Check{Name: "per-term archives have a second page", Site: s.BlogID, Status: statusIf(short > 0, Warn), Count: short,
			Detail: ifStr(short > 0, fmt.Sprintf("terms with more than posts_per_page (%d) posts in the dump keep %d or fewer", ppp, ppp))})
	}

	// References from kept posts to IDs the dump does not contain, by
	// where the reference was found.
	rows, err := db.QueryContext(f.ctx, `SELECT r.source, count(*) FROM (
			SELECT m.site, m.ref_id, fk.reason AS source FROM ix.meta_refs m
			JOIN follow_keys fk ON fk.site = m.site AND fk.meta_key = m.meta_key
			JOIN keep k ON k.site = m.site AND k.id = m.post_id
			WHERE m.meta_key <> '_menu_item_object_id'
			UNION ALL
			SELECT c.site, c.ref_id, 'content:' || c.source FROM ix.content_refs c JOIN keep k ON k.site = c.site AND k.id = c.post_id
		) r WHERE NOT EXISTS (SELECT 1 FROM ix.posts p WHERE p.site = r.site AND p.id = r.ref_id)
		GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err == nil {
		var never int64
		var parts []string
		for rows.Next() {
			var source string
			var n int64
			if rows.Scan(&source, &n) == nil {
				never += n
				if len(parts) < 5 {
					parts = append(parts, fmt.Sprintf("%s %d", source, n))
				}
			}
		}
		rows.Close()
		f.report.add(Check{Name: "references to posts never in the dump", Status: statusIf(never > 0, Warn), Count: never,
			Detail: ifStr(never > 0, "kept posts reference post IDs that the dump does not contain: "+strings.Join(parts, ", "))})
	}
	authors, err := planCount(f.ctx, db, `SELECT count(*) FROM keep k JOIN ix.posts p ON p.site = k.site AND p.id = k.id
		WHERE p.author > 0 AND p.author NOT IN (SELECT id FROM ix.users)`)
	if err == nil {
		f.report.add(Check{Name: "post authors exist", Status: statusIf(authors > 0, Warn), Count: authors,
			Detail: ifStr(authors > 0, "kept posts whose author was never in the dump, usually a deleted user")})
	}
}

func lower(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = strings.ToLower(s)
	}
	return out
}
