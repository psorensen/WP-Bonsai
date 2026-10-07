package plan

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/synth"
)

const testConfig = `
project: example-newspaper
post_types:
  post:
    mode: per_term
    per_term: 5
    taxonomies:
      category: { max_terms: all }
      post_tag: { max_terms: 10 }
    statuses: { publish: all, draft: 2 }
  page:
    mode: all
  product:
    mode: latest
    count: 7
  obituary:
    mode: none
tables:
  wp_example_log: empty
  wp_example_bylines: { filter_by: post_id }
`

var (
	indexMu    sync.Mutex
	indexPaths = map[bool]string{}
)

// testIndex builds a synthetic index, shared by the tests in this package.
// The multisite index adds a second site, blog 2, with prefix wp_2_.
func testIndex(t *testing.T, multisite bool) string {
	t.Helper()
	indexMu.Lock()
	defer indexMu.Unlock()
	if p, ok := indexPaths[multisite]; ok {
		return p
	}
	var dump bytes.Buffer
	if _, err := synth.Write(&dump, synth.Options{Seed: 31, Posts: 600, Subsite: multisite}); err != nil {
		t.Fatal(err)
	}
	dir, err := osMkdirTemp()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, index.FileName)
	if _, err := index.Build(context.Background(), &dump, path, index.Source{Path: "synthetic.sql"}, index.Options{}); err != nil {
		t.Fatal(err)
	}
	indexPaths[multisite] = path
	return path
}

func buildPlan(t *testing.T, cfgText string) *Plan {
	t.Helper()
	return buildPlanOn(t, false, cfgText)
}

func buildPlanOn(t *testing.T, multisite bool, cfgText string) *Plan {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), testIndex(t, multisite), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// count runs a query that returns one number.
func count(t *testing.T, p *Plan, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := p.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%v\nquery: %s", err, query)
	}
	return n
}

func TestSeeds(t *testing.T) {
	p := buildPlan(t, testConfig)

	if n := count(t, p, `SELECT count(*) FROM keep k JOIN ix.posts p USING (id) WHERE p.type = 'revision' OR p.status = 'auto-draft'`); n != 0 {
		t.Errorf("%d revisions or auto-drafts kept", n)
	}

	// latest 7: exactly the 7 newest published products.
	if n := count(t, p, `SELECT count(*) FROM seed WHERE reason = 'type:product:latest'`); n != 7 {
		t.Errorf("%d product seeds, want 7", n)
	}
	if n := count(t, p, `SELECT count(*) FROM seed s JOIN ix.posts p USING (id)
		WHERE s.reason = 'type:product:latest' AND p.date < (
			SELECT min(date) FROM (SELECT date FROM ix.posts WHERE type = 'product' AND status = 'publish' ORDER BY date DESC LIMIT 7))`); n != 0 {
		t.Errorf("%d product seeds are older than the 7 newest", n)
	}

	// per_term 5: every category holds min(5, eligible) seeded posts.
	if n := count(t, p, `WITH eligible AS (
			SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
			JOIN ix.posts p ON p.id = tr.object_id AND p.type = 'post' AND p.status = 'publish'
			JOIN ix.term_taxonomy tt USING (term_taxonomy_id) WHERE tt.taxonomy = 'category' GROUP BY 1
		), seeded AS (
			SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
			JOIN seed s ON s.id = tr.object_id AND s.reason = 'type:post:per_term:category' GROUP BY 1
		)
		SELECT count(*) FROM eligible e LEFT JOIN seeded s USING (term_taxonomy_id) WHERE coalesce(s.n, 0) <> least(e.n, 5)`); n != 0 {
		t.Errorf("%d categories do not hold min(5, eligible) seeded posts", n)
	}
	// max_terms 10: the 10 most used tags each hold at least min(5, eligible)
	// seeded posts. A post picked for one tag may carry another, so a tag can
	// hold more. No more than 50 posts are seeded through tags.
	if n := count(t, p, `WITH eligible AS (
			SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
			JOIN ix.posts p ON p.id = tr.object_id AND p.type = 'post' AND p.status = 'publish'
			JOIN ix.term_taxonomy tt USING (term_taxonomy_id) WHERE tt.taxonomy = 'post_tag'
			GROUP BY 1 ORDER BY n DESC, term_taxonomy_id LIMIT 10
		), seeded AS (
			SELECT tr.term_taxonomy_id, count(*) AS n FROM ix.term_relationships tr
			JOIN seed s ON s.id = tr.object_id AND s.reason = 'type:post:per_term:post_tag' GROUP BY 1
		)
		SELECT count(*) FROM eligible e LEFT JOIN seeded s USING (term_taxonomy_id) WHERE coalesce(s.n, 0) < least(e.n, 5)`); n != 0 {
		t.Errorf("%d of the 10 most used tags hold fewer than min(5, eligible) seeded posts", n)
	}
	if n := count(t, p, `SELECT count(*) FROM seed WHERE reason = 'type:post:per_term:post_tag'`); n > 50 || n == 0 {
		t.Errorf("%d posts seeded through tags, want 1 to 50", n)
	}
	if n := count(t, p, `SELECT count(*) FROM seed s JOIN ix.posts p USING (id) WHERE s.reason = 'type:post:status:draft' AND p.status = 'draft'`); n != 2 {
		t.Errorf("%d draft seeds, want 2", n)
	}
	if n := count(t, p, `SELECT count(*) FROM seed s JOIN ix.posts p USING (id) WHERE p.type = 'obituary'`); n != 0 {
		t.Errorf("%d obituary seeds with mode none", n)
	}
	if n := count(t, p, `SELECT count(*) FROM ix.posts WHERE type = 'page' AND status = 'publish' AND id NOT IN (SELECT id FROM keep)`); n != 0 {
		t.Errorf("%d published pages not kept with mode all", n)
	}
	if n := count(t, p, `SELECT count(*) FROM ix.posts WHERE type IN ('nav_menu_item', 'acf-field') AND id NOT IN (SELECT id FROM keep)`); n != 0 {
		t.Errorf("%d always-kept posts missing", n)
	}
	if n := count(t, p, `SELECT count(*) FROM keep k JOIN ix.options o ON o.name = 'page_on_front' AND k.id = CAST(o.value AS BIGINT)`); n != 1 {
		t.Error("page_on_front not kept")
	}
}

func TestDependencies(t *testing.T) {
	p := buildPlan(t, testConfig)

	// Featured images, menu targets, and parents are kept for every kept post.
	for name, q := range map[string]string{
		"featured image": `SELECT count(*) FROM ix.meta_refs m JOIN keep k ON k.id = m.post_id
			JOIN ix.posts t ON t.id = m.ref_id
			WHERE m.meta_key = '_thumbnail_id' AND m.ref_id NOT IN (SELECT id FROM keep)`,
		"menu target": `SELECT count(*) FROM ix.meta_refs m JOIN keep k ON k.id = m.post_id
			JOIN ix.posts t ON t.id = m.ref_id
			WHERE m.meta_key = '_menu_item_object_id' AND m.ref_id NOT IN (SELECT id FROM keep)`,
		"parent": `SELECT count(*) FROM keep k JOIN ix.posts p USING (id) JOIN ix.posts parent ON parent.id = p.parent
			WHERE p.type <> 'attachment' AND p.parent NOT IN (SELECT id FROM keep)`,
		"block image": `SELECT count(*) FROM ix.content_refs c JOIN keep k ON k.id = c.post_id
			JOIN ix.posts t ON t.id = c.ref_id WHERE c.ref_id NOT IN (SELECT id FROM keep)`,
		"ACF gallery": `SELECT count(*) FROM ix.meta_refs m JOIN keep k ON k.id = m.post_id
			JOIN ix.posts t ON t.id = m.ref_id
			WHERE m.meta_key = 'gallery' AND m.ref_id NOT IN (SELECT id FROM keep)`,
	} {
		if n := count(t, p, q); n != 0 {
			t.Errorf("%s: %d references from kept posts point at posts that were not kept", name, n)
		}
	}
	if !hasReason(p, "acf:related_story_id") || !hasReason(p, "acf:gallery") {
		t.Errorf("added = %v, want acf:related_story_id and acf:gallery", p.Added)
	}

	// With depth 0 only the exempt references are followed.
	p0 := buildPlan(t, strings.Replace(testConfig, "project: example-newspaper", "project: x\ndependency_depth: 0", 1))
	if hasReason(p0, "acf:related_story_id") {
		t.Error("depth 0 still followed related_story_id")
	}
	if !hasReason(p0, "content:block:core/image:id") {
		t.Error("depth 0 dropped attachments, which are followed past the depth limit")
	}
}

func hasReason(p *Plan, reason string) bool {
	for _, a := range p.Added {
		if a.Reason == reason {
			return true
		}
	}
	return false
}

func TestFilters(t *testing.T) {
	p := buildPlan(t, testConfig)

	// Every meta row of a kept post is kept, noise keys included.
	if kept, all := count(t, p, `SELECT count(*) FROM keep_postmeta`),
		count(t, p, `SELECT count(*) FROM ix.postmeta m WHERE m.post_id IN (SELECT id FROM keep)`); kept != all || kept == 0 {
		t.Errorf("kept %d meta rows of kept posts, want all %d", kept, all)
	}
	if n := count(t, p, `SELECT count(*) FROM keep_postmeta JOIN ix.postmeta m USING (meta_id) WHERE m.post_id NOT IN (SELECT id FROM keep)`); n != 0 {
		t.Errorf("%d meta rows of dropped posts kept", n)
	}
	if n := count(t, p, `SELECT count(*) FROM keep_comments`); n != 0 {
		t.Errorf("%d comments kept; comments are always removed", n)
	}
	if n := count(t, p, `SELECT count(*) FROM ix.posts p JOIN keep USING (id) WHERE p.author NOT IN (SELECT id FROM keep_users)`); n != 0 {
		t.Errorf("%d kept posts have an author who was not kept", n)
	}
	if n := count(t, p, `SELECT count(*) FROM keep_users WHERE id = 1`); n != 1 {
		t.Error("the administrator was not kept")
	}
	if n := count(t, p, `SELECT count(*) FROM keep_options JOIN ix.options o USING (option_id) WHERE o.name LIKE '%transient%'`); n != 0 {
		t.Errorf("%d transients kept", n)
	}
	if n := count(t, p, `SELECT count(*) FROM keep_tr WHERE object_id NOT IN (SELECT id FROM keep)`); n != 0 {
		t.Errorf("%d term relationships of dropped posts kept", n)
	}

	tables := map[string]TablePlan{}
	var sum int64
	for _, tp := range p.Tables {
		tables[tp.Name] = tp
		sum += tp.Bytes
	}
	for _, name := range []string{"wp_comments", "wp_commentmeta"} {
		if tp := tables[name]; tp.Rule.Action != config.TableEmpty || tp.KeptRows != 0 {
			t.Errorf("%s = %+v, want emptied", name, tp)
		}
	}
	if r := tables["wp_example_log"].Rule; r.Action != config.TableEmpty || r.Source != "config" {
		t.Errorf("wp_example_log rule = %+v", r)
	}
	by := tables["wp_example_bylines"]
	if by.Rule.Action != config.TableFilter || by.Rule.Source != "config" || by.Approximate {
		t.Errorf("wp_example_bylines = %+v", by)
	}
	if want := count(t, p, `SELECT count(*) FROM ix.object_rows WHERE tbl = 'wp_example_bylines' AND object_id IN (SELECT id FROM keep)`); by.KeptRows != want || want == 0 {
		t.Errorf("bylines kept %d rows, want %d", by.KeptRows, want)
	}
	if p.EstimateBytes != sum+2048 {
		t.Errorf("estimate %d is not the sum of the tables %d", p.EstimateBytes, sum+2048)
	}
	if posts := tables["wp_posts"]; posts.KeptRows != count(t, p, `SELECT count(*) FROM keep`) {
		t.Errorf("wp_posts kept rows %d do not match the keep table", posts.KeptRows)
	}
}

func TestWarningsAndErrors(t *testing.T) {
	p := buildPlan(t, testConfig+`
references:
  extra_meta_keys: [no_such_key]
`)
	for _, want := range []string{
		"post_types.post.per_term is 5, not more than posts_per_page (10)",
		`no value of meta key "no_such_key"`,
	} {
		found := false
		for _, w := range p.Warnings {
			found = found || strings.Contains(w, want)
		}
		if !found {
			t.Errorf("no warning containing %q in %v", want, p.Warnings)
		}
	}

	small := buildPlan(t, "target_size_mb: 0.01\n")
	found := false
	for _, w := range small.Warnings {
		found = found || strings.Contains(w, "over the target")
	}
	if !found {
		t.Errorf("no over-target warning in %v", small.Warnings)
	}

	cfg, err := config.Parse([]byte("tables:\n  wp_example_bylines: { filter_by: story_id }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), testIndex(t, false), cfg); err == nil || !strings.Contains(err.Error(), "story_id") {
		t.Errorf("bad filter_by column: got %v", err)
	}
}

func TestMultisite(t *testing.T) {
	all := buildPlanOn(t, true, testConfig+`
sites:
  "2":
    post_types:
      post: { mode: all }
`)
	tables := func(p *Plan) map[string]TablePlan {
		m := map[string]TablePlan{}
		for _, tp := range p.Tables {
			m[tp.Name] = tp
		}
		return m
	}
	ta := tables(all)
	if len(all.Sites) != 2 || all.Sites[1].Excluded || all.Sites[1].KeptPosts != 3 {
		t.Errorf("sites = %+v", all.Sites)
	}
	if tp := ta["wp_2_posts"]; tp.Rule.Action != ActionCore || tp.Site != 2 || tp.KeptRows != 3 {
		t.Errorf("wp_2_posts = %+v", tp)
	}
	if tp := ta["wp_2_options"]; tp.Rule.Action != ActionCore || tp.KeptRows != 5 {
		t.Errorf("wp_2_options = %+v", tp)
	}
	if r := ta["wp_blogs"].Rule; r.Action != config.TableKeep || r.Source != "multisite" {
		t.Errorf("wp_blogs rule = %+v", r)
	}
	// Site 1 still gets its own keep set; IDs of the two sites never mix.
	if n := count(t, all, `SELECT count(*) FROM keep k JOIN ix.posts p ON p.site = k.site AND p.id = k.id WHERE k.site = 2 AND p.tbl <> 'wp_2_posts'`); n != 0 {
		t.Errorf("%d site 2 keep rows point outside wp_2_posts", n)
	}
	if n := count(t, all, `SELECT count(*) FROM keep_usermeta JOIN ix.usermeta u USING (umeta_id) WHERE u.meta_key = 'wp_2_capabilities'`); n != 1 {
		t.Errorf("site 2 capabilities kept %d times, want 1", n)
	}

	ex := buildPlanOn(t, true, testConfig+`
sites:
  "2": { exclude: true }
`)
	te := tables(ex)
	if !ex.Sites[1].Excluded || ex.Sites[1].KeptPosts != 0 {
		t.Errorf("excluded site = %+v", ex.Sites[1])
	}
	for _, name := range []string{"wp_2_posts", "wp_2_postmeta", "wp_2_options"} {
		if tp := te[name]; tp.Rule.Action != ActionDrop || tp.Rule.Source != "sites" || tp.Bytes != 0 {
			t.Errorf("%s = %+v", name, tp)
		}
	}
	if tp := te["wp_blogs"]; tp.Rule.Action != config.TableFilter || tp.Rule.IDs != "sites" || tp.KeptRows != 1 {
		t.Errorf("wp_blogs = %+v", tp)
	}
	if n := count(t, ex, `SELECT count(*) FROM keep_usermeta JOIN ix.usermeta u USING (umeta_id) WHERE u.meta_key LIKE 'wp\_2\_%' ESCAPE '\'`); n != 0 {
		t.Errorf("%d user meta rows of the excluded site kept", n)
	}
	if ex.EstimateBytes >= all.EstimateBytes {
		t.Errorf("excluding a site did not shrink the estimate: %d >= %d", ex.EstimateBytes, all.EstimateBytes)
	}
}

func TestOrphanSiteDropped(t *testing.T) {
	// Blog 2 is listed in wp_blogs in the synthetic dump. A site with tables
	// but no wp_blogs row is tested through the resolver: the plan marks
	// it excluded, and its tables are dropped.
	r := newResolver(config.Default(), "wp_", map[int]string{1: "wp_", 3: "wp_3_"}, map[int]bool{3: true})
	rule, err := r.resolve(tableInfo{name: "wp_3_posts", role: "posts", site: 3})
	if err != nil || rule.Action != ActionDrop {
		t.Errorf("wp_3_posts rule = %+v, %v", rule, err)
	}
	rule, _ = r.resolve(tableInfo{name: "wp_3_yoast_indexable", site: 3, rowBytes: 10})
	if rule.Action != ActionDrop {
		t.Errorf("plugin table of an excluded site = %+v", rule)
	}
}

func TestLibraryRules(t *testing.T) {
	r := newResolver(config.Default(), "wp_", map[int]string{1: "wp_", 11: "wp_11_"}, map[int]bool{})
	for name, want := range map[string]string{
		"wp_11_rg_incomplete_submissions": config.TableEmpty,
		"wp_11_rg_form_view":              config.TableEmpty,
		"wp_11_rg_lead_detail":            config.TableEmpty,
		"wp_11_rg_form":                   config.TableKeep,
		"wp_gf_entry":                     config.TableEmpty,
		"wp_yoast_indexable":              config.TableEmpty,
		"wp_yoast_migrations":             config.TableKeep,
	} {
		site := 1
		if strings.HasPrefix(name, "wp_11_") {
			site = 11
		}
		rule, err := r.resolve(tableInfo{name: name, site: site, rowBytes: 10})
		if err != nil || rule.Action != want || !strings.HasPrefix(rule.Source, "library:") {
			t.Errorf("%s: rule %+v, %v; want %s from the library", name, rule, err, want)
		}
	}
}

func TestOptionRules(t *testing.T) {
	p := buildPlan(t, testConfig+`
options:
  exclude: ["big_*"]
`)
	for name, want := range map[string]int64{
		"_transient_feed_abc123": 0, "big_option": 0, "siteurl": 1, "posts_per_page": 1,
	} {
		if n := count(t, p, `SELECT count(*) FROM keep_options JOIN ix.options o USING (site, option_id) WHERE o.name = ?`, name); n != want {
			t.Errorf("option %s kept %d times, want %d", name, n, want)
		}
	}
	// Without the exclusion, the 120 KB big_option stays and no warning
	// names it, because it is under 1 MB.
	p2 := buildPlan(t, testConfig)
	if n := count(t, p2, `SELECT count(*) FROM keep_options JOIN ix.options o USING (site, option_id) WHERE o.name = 'big_option'`); n != 1 {
		t.Errorf("big_option kept %d times without an exclusion", n)
	}
	if got := globToLike("jpsq_*"); got != `jpsq\_%` {
		t.Errorf("jpsq pattern = %s", got)
	}
}

func TestGlobToLike(t *testing.T) {
	for glob, want := range map[string]string{
		"_oembed_*":  `\_oembed\_%`,
		"100%_done*": `100\%\_done%`,
		`back\slash`: `back\\slash`,
	} {
		if got := globToLike(glob); got != want {
			t.Errorf("globToLike(%q) = %q, want %q", glob, got, want)
		}
	}
}

var tempDirs []string

func osMkdirTemp() (string, error) {
	dir, err := os.MkdirTemp("", "bonsai-plan-test-")
	if err == nil {
		tempDirs = append(tempDirs, dir)
	}
	return dir, err
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, d := range tempDirs {
		os.RemoveAll(d)
	}
	os.Exit(code)
}
