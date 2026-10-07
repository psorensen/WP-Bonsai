package index

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/synth"
)

func buildIndex(t *testing.T, o synth.Options) (*Inventory, synth.Stats, string) {
	t.Helper()
	var dump bytes.Buffer
	stats, err := synth.Write(&dump, o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	size := int64(dump.Len())
	if _, err := Build(context.Background(), &dump, path, Source{Path: "synthetic.sql", Size: size}, Options{}); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	inv, err := ReadInventory(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return inv, stats, path
}

func TestInventory(t *testing.T) {
	variants := map[string]synth.Options{
		"mysqldump":             {Seed: 21, Posts: 300, Subsite: true},
		"complete-insert-small": {Seed: 22, Posts: 300, CompleteInsert: true, HexBlob: true, MaxInsertBytes: 4096, Triggers: true},
	}
	for name, o := range variants {
		t.Run(name, func(t *testing.T) {
			inv, stats, _ := buildIndex(t, o)

			if inv.Prefix != "wp_" {
				t.Errorf("prefix = %q", inv.Prefix)
			}
			if inv.Multisite != o.Subsite {
				t.Errorf("multisite = %v, want %v", inv.Multisite, o.Subsite)
			}

			// Every table in the dump, with its exact row count.
			tables := map[string]Table{}
			for _, tb := range inv.Tables {
				tables[tb.Name] = tb
			}
			for name, rows := range stats {
				if tables[name].Rows != int64(rows) {
					t.Errorf("table %s: %d rows, want %d", name, tables[name].Rows, rows)
				}
			}
			if r := tables["wp_postmeta"].Role; r != "postmeta" {
				t.Errorf("wp_postmeta role = %q", r)
			}
			if r := tables["wp_example_log"].Role; r != "" {
				t.Errorf("wp_example_log role = %q", r)
			}

			wantSites := 1
			if o.Subsite {
				wantSites = 2
			}
			if len(inv.Sites) != wantSites {
				t.Fatalf("%d sites, want %d", len(inv.Sites), wantSites)
			}
			site := &inv.Sites[0]
			if site.BlogID != 1 || site.Prefix != "wp_" {
				t.Errorf("site 1 = %d %q", site.BlogID, site.Prefix)
			}
			if o.Subsite {
				sub := inv.Sites[1]
				if sub.BlogID != 2 || sub.Prefix != "wp_2_" || sub.Path != "/sports/" || len(sub.PostTypes) != 1 || sub.PostTypes[0].Count != 3 {
					t.Errorf("site 2 = %+v", sub)
				}
				if sub.Options.PostsPerPage == nil || *sub.Options.PostsPerPage != 5 {
					t.Errorf("site 2 posts_per_page = %v", sub.Options.PostsPerPage)
				}
			}

			// Post counts of site 1 come only from wp_posts.
			var posts int64
			for _, pt := range site.PostTypes {
				posts += pt.Count
			}
			if posts != int64(stats["wp_posts"]) {
				t.Errorf("post types add up to %d posts, want %d", posts, stats["wp_posts"])
			}
			pt := findType(site, "post")
			if pt == nil || pt.Statuses["publish"] == 0 || pt.Statuses["pitch"] == 0 || pt.FirstDate == nil || pt.MetaBytes == 0 {
				t.Errorf("post type 'post' = %+v", pt)
			}
			if findType(site, "acf-field") == nil || findType(site, "revision") == nil {
				t.Error("acf-field or revision post type missing")
			}

			if site.ACFFields["gallery"] != 1 || site.ACFFields["post_object"] != 1 {
				t.Errorf("ACF fields = %v", site.ACFFields)
			}

			tax := map[string]Taxonomy{}
			for _, tx := range site.Taxonomies {
				tax[tx.Taxonomy] = tx
			}
			if c := tax["category"]; c.Terms != 12 || !c.Hierarchical || c.PostTypes["post"] == 0 || len(c.LargestTerms) != 5 {
				t.Errorf("category = %+v", c)
			}
			if tg := tax["post_tag"]; tg.Terms != 40 || tg.Hierarchical {
				t.Errorf("post_tag = %+v", tg)
			}

			if site.Options.PostsPerPage == nil || *site.Options.PostsPerPage != 10 {
				t.Errorf("posts_per_page = %v", site.Options.PostsPerPage)
			}
			if len(site.Options.Large) != 1 || site.Options.Large[0].Name != "big_option" {
				t.Errorf("large options = %v", site.Options.Large)
			}
			if site.Options.TransientCount != 2 {
				t.Errorf("transients = %d", site.Options.TransientCount)
			}
			if _, ok := site.Options.Values["admin_email"]; ok {
				t.Error("index stored admin_email; option values outside the allow list must not be stored")
			}

			if inv.Users != 5 || site.Roles["administrator"] != 1 || site.Roles["author"] != 4 {
				t.Errorf("users = %d, roles = %v", inv.Users, site.Roles)
			}
			if site.Comments.Count != int64(stats["wp_comments"]) || site.Comments.ByStatus["approved"] != site.Comments.Count {
				t.Errorf("comments = %+v", site.Comments)
			}

			content := map[string]RefCount{}
			for _, rc := range site.References.Content {
				content[rc.Name] = rc
			}
			for _, src := range []string{"block:core/image:id", "class:wp-image"} {
				rc := content[src]
				if rc.Refs == 0 || rc.Resolved != rc.Refs {
					t.Errorf("content refs %s = %+v", src, rc)
				}
			}
			var keys []string
			for _, rc := range site.References.MetaKeys {
				keys = append(keys, rc.Name)
			}
			for _, k := range []string{"_thumbnail_id", "gallery", "related_story_id", "_menu_item_object_id"} {
				if !slices.Contains(keys, k) {
					t.Errorf("meta key %s not among references %v", k, keys)
				}
			}
		})
	}
}

func TestSiteColumns(t *testing.T) {
	_, stats, path := buildIndex(t, synth.Options{Seed: 23, Posts: 100, Subsite: true})
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, st := range siteTables {
		if n := q(`SELECT count(*) FROM ` + st.table + ` WHERE site IS NULL OR site NOT IN (1, 2)`); n != 0 {
			t.Errorf("%s has %d rows without a site", st.table, n)
		}
	}
	if n := q(`SELECT count(*) FROM posts WHERE site = 1`); n != stats["wp_posts"] {
		t.Errorf("site 1 has %d posts, want %d", n, stats["wp_posts"])
	}
	if n := q(`SELECT count(*) FROM posts WHERE site = 2 AND tbl = 'wp_2_posts'`); n != stats["wp_2_posts"] {
		t.Errorf("site 2 has %d posts, want %d", n, stats["wp_2_posts"])
	}
	if n := q(`SELECT count(*) FROM object_rows WHERE tbl = 'wp_example_bylines' AND site = 1 AND column_name = 'post_id'`); n != stats["wp_example_bylines"] {
		t.Errorf("object_rows has %d bylines rows for site 1, want %d", n, stats["wp_example_bylines"])
	}
	for name, want := range map[string]any{"wp_2_posts": 2, "wp_posts": 1, "wp_example_log": 1, "wp_users": nil, "wp_blogs": nil} {
		var site sql.NullInt64
		if err := db.QueryRow(`SELECT site FROM tables WHERE name = ?`, name).Scan(&site); err != nil {
			t.Fatal(err)
		}
		if (want == nil) != !site.Valid || (want != nil && int(site.Int64) != want.(int)) {
			t.Errorf("table %s site = %v, want %v", name, site, want)
		}
	}
}

func findType(site *Site, typ string) *PostType {
	for i := range site.PostTypes {
		if site.PostTypes[i].Type == typ {
			return &site.PostTypes[i]
		}
	}
	return nil
}
