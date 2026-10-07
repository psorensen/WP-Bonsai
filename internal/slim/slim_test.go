package slim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/mariadbtest"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/sqldump"
	"github.com/psorensen/WP-Bonsai/internal/synth"
)

const testConfig = `
post_types:
  post:
    mode: per_term
    per_term: 11
    taxonomies:
      category: { max_terms: all }
    statuses: { publish: all, draft: 2 }
  page: { mode: all }
  product: { mode: latest, count: 7 }
tables:
  wp_example_log: empty
  wp_example_bylines: { filter_by: post_id }
`

type run struct {
	dump []byte
	out  []byte
	plan *plan.Plan
	res  *Result
}

// build runs pass 1, the plan, and pass 2 on a synthetic dump.
func build(t *testing.T, o synth.Options, cfgText string, maxInsert int) *run {
	t.Helper()
	var dump bytes.Buffer
	if _, err := synth.Write(&dump, o); err != nil {
		t.Fatal(err)
	}
	r := &run{dump: dump.Bytes()}
	path := filepath.Join(t.TempDir(), index.FileName)
	if _, err := index.Build(context.Background(), bytes.NewReader(r.dump), path, index.Source{}, index.Options{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	r.plan, err = plan.Build(context.Background(), path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.plan.Close() })
	keep, err := r.plan.LoadKeepSets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.res, err = Write(context.Background(), bytes.NewReader(r.dump), &out, r.plan, keep, Options{MaxInsertBytes: maxInsert})
	if err != nil {
		t.Fatal(err)
	}
	r.out = out.Bytes()
	return r
}

// parsed counts what the output holds per table.
type parsed struct {
	rows     map[string]int
	creates  map[string]bool
	mentions map[string]bool // any statement naming the table
	triggers map[string]bool
}

func parse(t *testing.T, b []byte) parsed {
	t.Helper()
	ps := parsed{rows: map[string]int{}, creates: map[string]bool{}, mentions: map[string]bool{}, triggers: map[string]bool{}}
	p := sqldump.NewParser(bytes.NewReader(b))
	for {
		it, err := p.Next()
		if errors.Is(err, io.EOF) {
			return ps
		}
		if err != nil {
			t.Fatalf("output does not parse: %v", err)
		}
		if it.Table != "" {
			ps.mentions[it.Table] = true
		}
		switch {
		case it.Kind == sqldump.Row:
			ps.rows[it.Table]++
		case it.Type == sqldump.StmtCreateTable:
			ps.creates[it.Table] = true
		case it.Type == sqldump.StmtCreateTrigger:
			ps.triggers[it.Table] = true
		}
	}
}

func TestWrite(t *testing.T) {
	r := build(t, synth.Options{Seed: 41, Posts: 800, Subsite: true, Triggers: true, MaxInsertBytes: 8192},
		testConfig+`sites: {"2": {exclude: true}}`, 4096)
	if len(r.res.Mismatches) > 0 {
		t.Fatalf("mismatches: %v", r.res.Mismatches)
	}
	out := parse(t, r.out)

	for _, tp := range r.plan.Tables {
		if got := out.rows[tp.Name]; int64(got) != tp.KeptRows && !tp.Approximate {
			t.Errorf("%s: output has %d rows, plan kept %d", tp.Name, got, tp.KeptRows)
		}
		switch tp.Rule.Action {
		case plan.ActionDrop:
			if out.mentions[tp.Name] || out.triggers[tp.Name] {
				t.Errorf("dropped table %s still appears in the output", tp.Name)
			}
		default:
			if !out.creates[tp.Name] {
				t.Errorf("table %s has no CREATE TABLE in the output", tp.Name)
			}
		}
	}
	if out.rows["wp_example_log"] != 0 || !out.creates["wp_example_log"] {
		t.Error("emptied table wp_example_log should keep its schema and lose its rows")
	}
	if out.rows["wp_blogs"] != 1 {
		t.Errorf("wp_blogs has %d rows, want 1 after excluding site 2", out.rows["wp_blogs"])
	}
	if !out.triggers["wp_posts"] {
		t.Error("the trigger on wp_posts is missing")
	}
	if out.triggers["wp_2_posts"] {
		t.Error("the trigger on the dropped wp_2_posts is still there")
	}

	// The estimate comes from row sizes, so it should be close.
	ratio := float64(r.res.Bytes) / float64(r.plan.EstimateBytes)
	if ratio < 0.9 || ratio > 1.1 {
		t.Errorf("wrote %d bytes, estimate %d (ratio %.2f)", r.res.Bytes, r.plan.EstimateBytes, ratio)
	}
	if r.res.Bytes*2 > int64(len(r.dump)) {
		t.Errorf("output %d bytes is not much smaller than the dump %d", r.res.Bytes, len(r.dump))
	}
}

// TestMariaDB imports the slim dump and checks that it holds together.
func TestMariaDB(t *testing.T) {
	mariadbtest.Skip(t)
	db := mariadbtest.Start(t)

	for name, c := range map[string]struct {
		o   synth.Options
		cfg string
	}{
		"single-site":     {synth.Options{Seed: 42, Posts: 800, Triggers: true}, testConfig},
		"multisite":       {synth.Options{Seed: 43, Posts: 800, Subsite: true, CompleteInsert: true, HexBlob: true}, testConfig},
		"site-2-excluded": {synth.Options{Seed: 44, Posts: 800, Subsite: true, Triggers: true}, testConfig + `sites: {"2": {exclude: true}}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := build(t, c.o, c.cfg, 0)
			if len(r.res.Mismatches) > 0 {
				t.Fatalf("mismatches: %v", r.res.Mismatches)
			}
			schema := strings.ReplaceAll(name, "-", "_")
			db.Import(t, schema, r.out)

			for _, tp := range r.plan.Tables {
				if tp.Rule.Action == plan.ActionDrop {
					if got := db.Query(t, fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema = '%s' AND table_name = '%s'", schema, tp.Name)); got != "0" {
						t.Errorf("dropped table %s exists", tp.Name)
					}
					continue
				}
				got := db.Query(t, fmt.Sprintf("SELECT count(*) FROM `%s`.`%s`", schema, tp.Name))
				if got != fmt.Sprint(tp.KeptRows) {
					t.Errorf("%s: %s rows in MariaDB, plan kept %d", tp.Name, got, tp.KeptRows)
				}
			}

			for _, prefix := range sitePrefixes(r.plan) {
				checks := map[string]string{
					"meta of missing posts": `SELECT count(*) FROM %[1]spostmeta m LEFT JOIN %[1]sposts p ON p.ID = m.post_id WHERE p.ID IS NULL`,
					"relationships of missing posts": `SELECT count(*) FROM %[1]sterm_relationships r
						JOIN %[1]sterm_taxonomy tt USING (term_taxonomy_id)
						LEFT JOIN %[1]sposts p ON p.ID = r.object_id WHERE p.ID IS NULL AND tt.taxonomy <> 'link_category'`,
					"relationships of missing terms": `SELECT count(*) FROM %[1]sterm_relationships r LEFT JOIN %[1]sterm_taxonomy tt USING (term_taxonomy_id) WHERE tt.term_taxonomy_id IS NULL`,
					"missing featured images":        `SELECT count(*) FROM %[1]spostmeta m LEFT JOIN %[1]sposts p ON p.ID = m.meta_value WHERE m.meta_key = '_thumbnail_id' AND p.ID IS NULL`,
					"missing menu targets":           `SELECT count(*) FROM %[1]spostmeta m LEFT JOIN %[1]sposts p ON p.ID = m.meta_value WHERE m.meta_key = '_menu_item_object_id' AND p.ID IS NULL`,
					"comments of missing posts":      `SELECT count(*) FROM %[1]scomments c LEFT JOIN %[1]sposts p ON p.ID = c.comment_post_ID WHERE p.ID IS NULL`,
					"missing front page":             `SELECT count(*) FROM %[1]soptions o LEFT JOIN %[1]sposts p ON p.ID = o.option_value WHERE o.option_name = 'page_on_front' AND o.option_value <> '0' AND p.ID IS NULL`,
					"missing authors":                `SELECT count(*) FROM %[1]sposts p LEFT JOIN wp_users u ON u.ID = p.post_author WHERE u.ID IS NULL`,
				}
				existing := db.Query(t, fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s'", schema))
				for what, q := range checks {
					// The synthetic subsite has no term or comment tables.
					missing := false
					for _, m := range tableRef.FindAllStringSubmatch(q, -1) {
						name := prefix + m[1]
						if m[0] == "wp_users" {
							name = "wp_users"
						}
						missing = missing || !slices.Contains(strings.Split(existing, "\n"), name)
					}
					if missing {
						continue
					}
					if got := db.Query(t, "USE `"+schema+"`; "+fmt.Sprintf(q, prefix)); got != "0" {
						t.Errorf("%s: %s rows with %s", prefix, got, what)
					}
				}
			}
		})
	}
}

// tableRef finds the tables a check query uses.
var tableRef = regexp.MustCompile(`%\[1\]s(\w+)|wp_users`)

func sitePrefixes(p *plan.Plan) []string {
	var out []string
	for _, s := range p.Sites {
		if !s.Excluded {
			out = append(out, s.Prefix)
		}
	}
	return out
}
