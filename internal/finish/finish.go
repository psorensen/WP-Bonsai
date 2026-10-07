// Package finish runs the sandbox finish step on the pass 2 output: import,
// scrub, admin login, term recount, option clean-up, validation, and export.
// See SPEC.md, "Finish in a sandbox" and "Validation".
//
// The output is written only when no check fails. If the scrubber fails,
// the whole step fails, so an unscrubbed file is never delivered.
package finish

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/phpser"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/sandbox"
)

// The known local admin login the finish step adds to every kept site.
const (
	AdminLogin    = "bonsai"
	AdminPassword = "bonsai"
	adminEmail    = "bonsai@example.com"
)

// scrubbedDomains are email domains that count as scrubbed: the dummy
// users WP Scrubber writes, and the domains it leaves alone by default.
var scrubbedDomains = []string{"example.com", "example.org", "example.net", "10up.com", "get10up.com"}

// Status of a check or a whole report.
const (
	Pass = "pass"
	Warn = "warning"
	Fail = "fail"
)

// Check is one validation result.
type Check struct {
	Name   string `json:"name"`
	Site   int    `json:"site,omitempty"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
	Detail string `json:"detail,omitempty"`
}

// Report is the validation report. It holds counts only, no data from the
// dump, so it is safe to share.
type Report struct {
	Status      string   `json:"status"`
	OutputBytes int64    `json:"output_bytes"`
	TargetBytes int64    `json:"target_bytes"`
	Sites       []int    `json:"sites"`
	Admin       string   `json:"admin_login"`
	Steps       []string `json:"steps"`
	Checks      []Check  `json:"checks"`
	// AfterImport lists commands to run after importing the dump locally,
	// with the project's plugins active, to rebuild emptied index tables.
	AfterImport []string `json:"after_import"`
}

func (r *Report) add(c Check) {
	r.Checks = append(r.Checks, c)
	switch {
	case c.Status == Fail:
		r.Status = Fail
	case c.Status == Warn && r.Status == Pass:
		r.Status = Warn
	}
}

// step records a finished step for the report and the log.
func (f *finisher) step(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	f.report.Steps = append(f.report.Steps, msg)
	f.log(msg)
}

// Options controls Run.
type Options struct {
	Log func(string)
}

type site struct {
	plan.SitePlan
	url string // for wp --url on a network
}

type finisher struct {
	ctx    context.Context
	sb     *sandbox.Sandbox
	p      *plan.Plan
	cfg    *config.Config
	log    func(string)
	report *Report

	sites     []site // kept sites
	multisite bool
	prefix    string
	tables    map[string]bool // tables in the sandbox database
}

// has reports whether every named table exists in the sandbox database.
func (f *finisher) has(names ...string) bool {
	for _, n := range names {
		if !f.tables[n] {
			return false
		}
	}
	return true
}

// Run finishes the pass 2 output at slimPath and writes the final dump to
// outPath. It returns the report even when a check fails; in that case it
// writes no output and returns an error too.
func Run(ctx context.Context, slimPath, outPath string, p *plan.Plan, cfg *config.Config, opts Options) (*Report, error) {
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	f := &finisher{ctx: ctx, p: p, cfg: cfg, log: opts.Log, prefix: p.Prefix,
		report: &Report{Status: Pass, TargetBytes: p.TargetBytes, Admin: AdminLogin, Checks: []Check{}, Steps: []string{}}}
	for _, s := range p.Sites {
		if !s.Excluded {
			f.sites = append(f.sites, site{SitePlan: s, url: s.Domain + s.Path})
			f.report.Sites = append(f.report.Sites, s.BlogID)
		}
	}
	f.report.AfterImport = afterImport(p)

	sb, err := sandbox.Start(ctx, opts.Log)
	if err != nil {
		return nil, err
	}
	defer sb.Close()
	f.sb = sb

	if err := f.run(slimPath); err != nil {
		f.report.add(Check{Name: "finish step", Status: Fail, Detail: err.Error()})
		return f.report, err
	}
	f.validate()
	if f.report.Status == Fail {
		return f.report, fmt.Errorf("validation failed; the output was not written")
	}
	if err := f.export(outPath); err != nil {
		return f.report, err
	}
	if f.report.OutputBytes > f.report.TargetBytes {
		f.report.add(Check{Name: "output under the target size", Status: Warn, Count: f.report.OutputBytes,
			Detail: fmt.Sprintf("%s is over the target %s", plan.FormatBytes(f.report.OutputBytes), plan.FormatBytes(f.report.TargetBytes))})
	} else {
		f.report.add(Check{Name: "output under the target size", Status: Pass, Count: f.report.OutputBytes})
	}
	return f.report, nil
}

func (f *finisher) sql(q string) (string, error) { return f.sb.SQL(f.ctx, q) }

// count runs a query that returns one number. A failed query fails the run.
func (f *finisher) count(q string) (int64, error) {
	out, err := f.sql(q)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

func (f *finisher) wp(args ...string) (string, error) {
	return f.sb.WP(f.ctx, append(args, "--path=/var/www/html")...)
}

// wpSite runs a WP-CLI command against one site of a network.
func (f *finisher) wpSite(s site, args ...string) (string, error) {
	if f.multisite {
		args = append(args, "--url="+s.url)
	}
	return f.wp(args...)
}

func (f *finisher) run(slimPath string) error {
	if err := f.sb.Import(f.ctx, slimPath); err != nil {
		return err
	}
	f.step("imported the pass 2 output into MariaDB")
	out, err := f.sql("SELECT table_name FROM information_schema.tables WHERE table_schema = 'wordpress'")
	if err != nil {
		return err
	}
	f.tables = map[string]bool{}
	for _, name := range strings.Fields(out) {
		f.tables[name] = true
	}

	if err := f.writeConfig(); err != nil {
		return err
	}

	// The scrub gate: if the scrubber fails, the run fails.
	args := []string{"scrub", "all", "--ignore-size-limit"}
	if len(f.cfg.Scrub.AllowedDomains) > 0 {
		args = append(args, "--allowed-domains="+strings.Join(f.cfg.Scrub.AllowedDomains, ","))
	}
	if len(f.cfg.Scrub.AllowedEmails) > 0 {
		args = append(args, "--allowed-emails="+strings.Join(f.cfg.Scrub.AllowedEmails, ","))
	}
	if _, err := f.wpSite(f.sites[0], args...); err != nil {
		return fmt.Errorf("scrubber failed: %w", err)
	}
	f.step("ran WP Scrubber: wp scrub all")

	if err := f.scrubExtras(); err != nil {
		return err
	}
	if err := f.addAdmin(); err != nil {
		return err
	}
	if err := f.recountTerms(); err != nil {
		return err
	}
	return f.fixOptions()
}

// writeConfig writes wp-config.php for the imported database.
func (f *finisher) writeConfig() error {
	if _, err := f.wp("config", "create", "--dbname=wordpress", "--dbuser=root", "--dbpass=", "--dbhost=db",
		"--dbprefix="+f.prefix, "--skip-check", "--force"); err != nil {
		return err
	}
	set := func(name, value string, raw bool) error {
		args := []string{"config", "set", name, value}
		if raw {
			args = append(args, "--raw")
		}
		_, err := f.wp(args...)
		return err
	}
	// WP Scrubber refuses to run on production.
	for _, c := range [][3]string{
		{"WP_ENVIRONMENT_TYPE", "local", ""},
		{"DISABLE_WP_CRON", "true", "raw"},
		{"WP_HTTP_BLOCK_EXTERNAL", "true", "raw"},
	} {
		if err := set(c[0], c[1], c[2] == "raw"); err != nil {
			return err
		}
	}

	f.multisite = f.has(f.prefix + "site")
	if !f.multisite {
		return nil
	}
	row, err := f.sql(fmt.Sprintf("SELECT domain, path FROM %ssite WHERE id = 1", f.prefix))
	if err != nil {
		return err
	}
	domain, path, _ := strings.Cut(row, "\t")
	sub, err := f.sql(fmt.Sprintf("SELECT coalesce(max(meta_value), '0') FROM %ssitemeta WHERE meta_key = 'subdomain_install'", f.prefix))
	if err != nil {
		return err
	}
	for _, c := range [][3]string{
		{"MULTISITE", "true", "raw"},
		{"SUBDOMAIN_INSTALL", strconv.FormatBool(sub == "1"), "raw"},
		{"DOMAIN_CURRENT_SITE", domain, ""},
		{"PATH_CURRENT_SITE", path, ""},
		{"SITE_ID_CURRENT_SITE", "1", "raw"},
		{"BLOG_ID_CURRENT_SITE", "1", "raw"},
	} {
		if err := set(c[0], c[1], c[2] == "raw"); err != nil {
			return err
		}
	}
	f.step("wrote wp-config.php for a network of %d kept sites", len(f.sites))
	return nil
}

// scrubExtras replaces the personal data WP Scrubber does not touch: the
// admin email of each site and of the network.
func (f *finisher) scrubExtras() error {
	var stmts []string
	for _, s := range f.sites {
		if f.has(s.Prefix + "options") {
			stmts = append(stmts, fmt.Sprintf(
				"UPDATE %soptions SET option_value = 'admin@example.com' WHERE option_name IN ('admin_email', 'new_admin_email');", s.Prefix))
		}
	}
	if len(stmts) == 0 && !f.multisite {
		return nil
	}
	if f.multisite {
		stmts = append(stmts,
			fmt.Sprintf("UPDATE %ssitemeta SET meta_value = 'admin@example.com' WHERE meta_key IN ('admin_email', 'new_admin_email');", f.prefix),
			fmt.Sprintf("DELETE FROM %ssitemeta WHERE meta_key LIKE '\\_site\\_transient\\_%%';", f.prefix))
	}
	if _, err := f.sql(strings.Join(stmts, "\n")); err != nil {
		return err
	}
	f.step("replaced site and network admin emails with admin@example.com")
	return nil
}

// addAdmin adds the known local admin login to every kept site.
func (f *finisher) addAdmin() error {
	if _, err := f.wpSite(f.sites[0], "user", "create", AdminLogin, adminEmail,
		"--role=administrator", "--user_pass="+AdminPassword, "--porcelain"); err != nil {
		return err
	}
	if f.multisite {
		// The scrubber renamed every user, so the old super admin list
		// names logins that no longer exist.
		if _, err := f.wpSite(f.sites[0], "network", "meta", "update", "1", "site_admins",
			fmt.Sprintf(`[%q]`, AdminLogin), "--format=json"); err != nil {
			return err
		}
		for _, s := range f.sites[1:] {
			if _, err := f.wpSite(s, "user", "set-role", AdminLogin, "administrator"); err != nil {
				return err
			}
		}
	}
	f.step("added the local admin login %q (password %q) to every kept site", AdminLogin, AdminPassword)
	return nil
}

// genericCountTypes are post types whose terms WordPress counts whatever
// their status, through _update_generic_term_count.
var genericCountTypes = []string{"nav_menu_item", "wp_template", "wp_template_part", "wp_navigation", "wp_global_styles", "wp_block"}

// recountTerms sets term counts from the kept rows, as wp term recount
// would. It runs in SQL because the site's own taxonomies are not
// registered in the sandbox, where the theme and plugins are missing.
func (f *finisher) recountTerms() error {
	var stmts []string
	for _, s := range f.sites {
		p := s.Prefix
		if !f.has(p+"term_taxonomy", p+"term_relationships", p+"posts") {
			continue
		}
		stmts = append(stmts, fmt.Sprintf(`
UPDATE %[1]sterm_taxonomy tt SET count = (
	SELECT count(*) FROM %[1]sterm_relationships tr JOIN %[1]sposts p ON p.ID = tr.object_id
	WHERE tr.term_taxonomy_id = tt.term_taxonomy_id
		AND (p.post_status = 'publish' OR (p.post_type = 'attachment' AND p.post_status = 'inherit') OR p.post_type IN (%[2]s))
)
WHERE tt.taxonomy <> 'link_category' AND NOT EXISTS (
	SELECT 1 FROM %[1]sterm_relationships tr2 JOIN %[1]sterm_taxonomy tt2 USING (term_taxonomy_id)
	LEFT JOIN %[1]sposts p2 ON p2.ID = tr2.object_id
	WHERE tt2.taxonomy = tt.taxonomy AND p2.ID IS NULL
);`, p, sqlList(genericCountTypes)))
	}
	if len(stmts) > 0 {
		if _, err := f.sql(strings.Join(stmts, "\n")); err != nil {
			return err
		}
	}
	f.step("recounted terms on %d of %d kept sites", len(stmts), len(f.sites))
	return nil
}

// optionPageRefs are options that hold one post ID.
var optionPageRefs = []string{
	"page_on_front", "page_for_posts", "wp_page_for_privacy_policy",
	"woocommerce_shop_page_id", "woocommerce_cart_page_id", "woocommerce_checkout_page_id",
	"woocommerce_myaccount_page_id", "woocommerce_terms_page_id",
}

// fixOptions clears option references to posts that are not in the output.
// The plan keeps every page these options name, so a missing page was never
// in the dump.
func (f *finisher) fixOptions() error {
	for _, s := range f.sites {
		p := s.Prefix
		if !f.has(p+"options", p+"posts") {
			continue
		}
		missing, err := f.count(fmt.Sprintf(`SELECT count(*) FROM %[1]soptions o LEFT JOIN %[1]sposts p ON p.ID = o.option_value
			WHERE o.option_name IN (%[2]s) AND o.option_value REGEXP '^[0-9]+$' AND o.option_value <> '0' AND p.ID IS NULL`, p, sqlList(optionPageRefs)))
		if err != nil {
			return err
		}
		if missing > 0 {
			if _, err := f.sql(fmt.Sprintf(`UPDATE %[1]soptions o LEFT JOIN %[1]sposts p ON p.ID = o.option_value SET o.option_value = '0'
				WHERE o.option_name IN (%[2]s) AND o.option_value REGEXP '^[0-9]+$' AND o.option_value <> '0' AND p.ID IS NULL`, p, sqlList(optionPageRefs))); err != nil {
				return err
			}
		}
		f.report.add(Check{Name: "page options point at kept pages", Site: s.BlogID, Status: statusIf(missing > 0, Warn), Count: missing,
			Detail: ifStr(missing > 0, "these options named pages that were never in the dump; they are reset to 0")})

		raw, err := f.sql(fmt.Sprintf("SELECT option_value FROM %soptions WHERE option_name = 'sticky_posts'", p))
		if err != nil || raw == "" {
			continue
		}
		v, err := phpser.Unserialize([]byte(raw))
		if err != nil {
			continue
		}
		var ids []int64
		phpser.Walk(v, func(x phpser.Value) {
			if x.Kind == phpser.Int && x.Int > 0 {
				ids = append(ids, x.Int)
			}
		})
		var keep []int64
		for _, id := range ids {
			n, err := f.count(fmt.Sprintf("SELECT count(*) FROM %sposts WHERE ID = %d", p, id))
			if err != nil {
				return err
			}
			if n > 0 {
				keep = append(keep, id)
			}
		}
		if len(keep) != len(ids) {
			if _, err := f.sql(fmt.Sprintf("UPDATE %soptions SET option_value = '%s' WHERE option_name = 'sticky_posts'", p, phpser.SerializeInts(keep))); err != nil {
				return err
			}
		}
	}
	f.step("cleared option references to posts that are not in the output")
	return nil
}

func (f *finisher) export(outPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(outPath), ".bonsai-final-*.sql")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	cw := &countingWriter{w: tmp}
	if err := f.sb.Export(f.ctx, cw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), outPath); err != nil {
		return err
	}
	f.report.OutputBytes = cw.n
	f.step("exported the scrubbed database to %s", outPath)
	return nil
}

// afterImport collects the rebuild commands of emptied index tables.
func afterImport(p *plan.Plan) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range p.Tables {
		cmd, ok := strings.CutPrefix(t.Rule.Note, "rebuild after import: ")
		if ok && t.Rule.Action == config.TableEmpty && t.Rows > 0 && !seen[cmd] {
			seen[cmd] = true
			out = append(out, cmd)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

// WriteJSON writes the report as JSON.
func (r *Report) WriteJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// --- helpers ---

func statusIf(bad bool, status string) string {
	if bad {
		return status
	}
	return Pass
}

func ifStr(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

// sqlString quotes s as a MariaDB string literal.
func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func sqlList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = sqlString(s)
	}
	return strings.Join(q, ", ")
}

func sqlInts(ids []int64) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(q, ", ")
}

// planCount runs a query on the plan database that returns one number.
func planCount(ctx context.Context, db *sql.DB, q string, args ...any) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
