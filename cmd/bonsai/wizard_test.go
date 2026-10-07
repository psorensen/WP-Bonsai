package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/synth"
)

func testInventory(t *testing.T) (*index.Inventory, string) {
	t.Helper()
	var dump bytes.Buffer
	if _, err := synth.Write(&dump, synth.Options{Seed: 61, Posts: 1500, Subsite: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), index.FileName)
	if _, err := index.Build(context.Background(), &dump, path, index.Source{}, index.Options{}); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	inv, err := index.ReadInventory(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return inv, path
}

func TestSuggestTypes(t *testing.T) {
	inv, _ := testInventory(t)
	choices := suggestTypes(inv, map[int]bool{1: true, 2: true})
	byType := map[string]typeChoice{}
	for _, c := range choices {
		byType[c.Type] = c
	}
	// posts_per_page is 10 on site 1 and 5 on site 2; the larger wins.
	if c := byType["post"]; c.Taxonomy != "category" || c.Answer != "11" {
		t.Errorf("post = %+v, want 11 per category", c)
	}
	if c := byType["page"]; c.Taxonomy != "" || c.Answer != "20" {
		t.Errorf("page = %+v, want latest 20", c)
	}
	for _, typ := range []string{"attachment", "revision", "nav_menu_item", "acf-field"} {
		if _, ok := byType[typ]; ok {
			t.Errorf("%s should not be asked about", typ)
		}
	}
	if choices[0].Type != "post" {
		t.Errorf("biggest type first, got %s", choices[0].Type)
	}
}

func TestWizardYAML(t *testing.T) {
	inv, indexPath := testInventory(t)
	choices := []typeChoice{
		{Type: "post", Taxonomy: "category", Answer: "11"},
		{Type: "page", Answer: "all"},
		{Type: "product", Answer: "25"},
		{Type: "obituary", Answer: "0"},
		{Type: "event", Answer: "", Default: "20"}, // blank keeps the default
	}
	text := wizardYAML("prod.sql.gz", 50, choices, inv, map[int]bool{1: true})
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatalf("wizard YAML does not parse: %v\n%s", err, text)
	}
	if cfg.TargetSizeMB != 50 {
		t.Errorf("target = %v", cfg.TargetSizeMB)
	}
	if pt := cfg.PostTypes["post"]; pt.Mode != config.ModePerTerm || pt.PerTerm != 11 || !pt.Taxonomies["category"].MaxTerms.All {
		t.Errorf("post = %+v", pt)
	}
	if pt := cfg.PostTypes["product"]; pt.Mode != config.ModeLatest || pt.Count != 25 {
		t.Errorf("product = %+v", pt)
	}
	if pt := cfg.PostTypes["event"]; pt.Mode != config.ModeLatest || pt.Count != 20 {
		t.Errorf("event = %+v, want the default latest 20", pt)
	}
	if cfg.PostTypes["page"].Mode != config.ModeAll || cfg.PostTypes["obituary"].Mode != config.ModeNone {
		t.Errorf("page/obituary = %+v %+v", cfg.PostTypes["page"], cfg.PostTypes["obituary"])
	}
	if !cfg.ForSite(2).Exclude || cfg.ForSite(1).Exclude {
		t.Errorf("site 2 should be excluded:\n%s", text)
	}

	// The config works end to end with the planner.
	p, err := plan.Build(context.Background(), indexPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Sites[1].Excluded {
		t.Errorf("plan kept site 2")
	}
}

func TestOutputNameAndDumpDetection(t *testing.T) {
	for in, want := range map[string]string{
		"prod.sql":              "prod-bonsai.sql",
		"/x/backup-2026.sql.gz": "backup-2026-bonsai.sql",
		"dump":                  "dump-bonsai.sql",
	} {
		if got := outputName(in); got != want {
			t.Errorf("outputName(%q) = %q, want %q", in, got, want)
		}
	}
	f := filepath.Join(t.TempDir(), "mydump")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for arg, want := range map[string]bool{
		"prod.sql": true, "prod.sql.gz": true, f: true,
		"build": false, "plan": false, "-h": false, t.TempDir(): false,
	} {
		if got := isDumpFile(arg); got != want {
			t.Errorf("isDumpFile(%q) = %v, want %v", arg, got, want)
		}
	}
	for _, s := range []string{"all", "ALL", "0", "25", " 7 ", ""} {
		if validAnswer(s) != nil {
			t.Errorf("validAnswer(%q) rejected", s)
		}
	}
	for _, s := range []string{"-1", "lots"} {
		if validAnswer(s) == nil {
			t.Errorf("validAnswer(%q) accepted", s)
		}
	}
	if !strings.Contains(yamlKey("acf-field"), "acf-field") || yamlKey("my type") != `"my type"` {
		t.Error("yamlKey quoting")
	}
}
