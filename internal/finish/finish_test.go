package finish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/mariadbtest"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/slim"
	"github.com/psorensen/WP-Bonsai/internal/synth"
)

const testConfig = `
post_types:
  post:
    mode: per_term
    per_term: 11
    taxonomies:
      category: { max_terms: all }
  page: { mode: all }
`

// finishDump runs pass 1, the plan, pass 2, and the finish step.
func finishDump(t *testing.T, o synth.Options, cfgText string) (*Report, string, error) {
	t.Helper()
	dir := t.TempDir()
	var dump bytes.Buffer
	if _, err := synth.Write(&dump, o); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(dir, index.FileName)
	if _, err := index.Build(context.Background(), bytes.NewReader(dump.Bytes()), idx, index.Source{}, index.Options{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(context.Background(), idx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	keep, err := p.LoadKeepSets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pass2 := filepath.Join(dir, "pass2.sql")
	f, err := os.Create(pass2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slim.Write(context.Background(), bytes.NewReader(dump.Bytes()), f, p, keep, slim.Options{}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(dir, "slim.sql")
	report, err := Run(context.Background(), pass2, out, p, cfg, Options{Log: func(m string) { t.Log(m) }})
	return report, out, err
}

func TestFinish(t *testing.T) {
	mariadbtest.Skip(t)

	report, out, err := finishDump(t, synth.Options{Seed: 51, Posts: 600, Subsite: true, Triggers: true},
		testConfig+"local: {url: \"https://example.test\"}\n")
	if err != nil {
		t.Fatalf("finish failed: %v\nreport: %+v", err, report)
	}
	if report.Status == Fail {
		t.Fatalf("report status %s: %+v", report.Status, report.Checks)
	}
	for _, c := range report.Checks {
		if c.Status == Fail {
			t.Errorf("check failed: %+v", c)
		}
	}
	if report.OutputBytes == 0 || len(report.AfterImport) != 0 {
		t.Errorf("report = %+v", report)
	}

	// Import the final file into a fresh server and look at what it holds.
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	db := mariadbtest.Start(t)
	db.Import(t, "final", b)
	q := func(sql string) string { return db.Query(t, "USE final; "+sql) }

	if n := q(`SELECT count(*) FROM wp_users WHERE user_email LIKE '%example-newspaper.test'`); n != "0" {
		t.Errorf("%s users still have their production email", n)
	}
	if n := q(`SELECT count(*) FROM wp_users WHERE user_login IN ('admin', 'author1', 'author2')`); n != "0" {
		t.Errorf("%s users still have their production login", n)
	}
	if n := q(`SELECT count(*) FROM wp_users WHERE user_login = 'bonsai'`); n != "1" {
		t.Errorf("bonsai admin rows: %s", n)
	}
	if v := q(`SELECT meta_value FROM wp_sitemeta WHERE meta_key = 'site_admins'`); !strings.Contains(v, `"bonsai"`) || strings.Contains(v, `"admin"`) {
		t.Errorf("site_admins = %s", v)
	}
	for _, p := range []string{"wp_", "wp_2_"} {
		if v := q(fmt.Sprintf(`SELECT option_value FROM %soptions WHERE option_name = 'admin_email'`, p)); v != "admin@example.com" {
			t.Errorf("%s admin_email = %q", p, v)
		}
		if n := q(fmt.Sprintf(`SELECT count(*) FROM wp_usermeta WHERE user_id = (SELECT ID FROM wp_users WHERE user_login = 'bonsai')
			AND meta_key = '%scapabilities' AND meta_value LIKE '%%administrator%%'`, p)); n != "1" {
			t.Errorf("bonsai is not an administrator of %s", p)
		}
	}
	// Local addresses.
	if v := q(`SELECT group_concat(concat(blog_id, ' ', domain, path) ORDER BY blog_id SEPARATOR ', ') FROM wp_blogs`); v != "1 example.test/, 2 example.test/sports/" {
		t.Errorf("wp_blogs = %s", v)
	}
	if v := q(`SELECT concat(domain, path) FROM wp_site`); v != "example.test/" {
		t.Errorf("wp_site = %s", v)
	}
	for p, want := range map[string]string{"wp_": "https://example.test", "wp_2_": "https://example.test/sports"} {
		if v := q(fmt.Sprintf(`SELECT option_value FROM %soptions WHERE option_name = 'home'`, p)); v != want {
			t.Errorf("%s home = %s, want %s", p, v, want)
		}
	}
	if n := q(`SELECT count(*) FROM wp_posts WHERE post_content LIKE '%example-newspaper.test%'`); n != "0" {
		t.Errorf("%s posts still link to the production domain", n)
	}
	if n := q(`SELECT count(*) FROM wp_posts WHERE guid LIKE '%example-newspaper.test%'`); n == "0" {
		t.Error("guid was rewritten; it must keep the production address")
	}
	if len(report.Local) != 2 || report.Local[1].To != "https://example.test/sports" {
		t.Errorf("report.Local = %+v", report.Local)
	}
	if n := q(`SELECT (SELECT count(*) FROM wp_comments) + (SELECT count(*) FROM wp_commentmeta)`); n != "0" {
		t.Errorf("%s comment rows remain", n)
	}
	// Term counts match the kept published posts.
	if n := q(`SELECT count(*) FROM wp_term_taxonomy tt WHERE tt.taxonomy = 'category' AND tt.count <> (
		SELECT count(*) FROM wp_term_relationships tr JOIN wp_posts p ON p.ID = tr.object_id
		WHERE tr.term_taxonomy_id = tt.term_taxonomy_id AND p.post_status = 'publish')`); n != "0" {
		t.Errorf("%s categories have a wrong count", n)
	}
	if n := q(`SELECT count(*) FROM information_schema.triggers WHERE trigger_schema = 'final'`); n != "2" {
		t.Errorf("%s triggers in the output, want 2", n)
	}
}

func TestFinishFailsOnUnscrubbedTable(t *testing.T) {
	mariadbtest.Skip(t)

	report, out, err := finishDump(t, synth.Options{Seed: 52, Posts: 200, PIITable: true}, testConfig)
	if err == nil {
		t.Fatal("finish succeeded with a table of subscriber emails and IP addresses")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("the output was written although validation failed")
	}
	var emails, ips bool
	for _, c := range report.Checks {
		if c.Status != Fail {
			continue
		}
		emails = emails || (strings.Contains(c.Name, "email") && c.Count == 20)
		ips = ips || (strings.Contains(c.Name, "IP") && c.Count == 20)
	}
	if !emails || !ips {
		t.Errorf("want failed email and IP checks with 20 rows each; checks: %+v", report.Checks)
	}

	// With a rule that empties the table, the same dump passes.
	report, _, err = finishDump(t, synth.Options{Seed: 52, Posts: 200, PIITable: true},
		testConfig+"tables:\n  wp_example_subscribers: empty\n")
	if err != nil {
		t.Fatalf("finish failed with the table emptied: %v\n%+v", err, report.Checks)
	}
}
