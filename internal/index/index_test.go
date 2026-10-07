package index

import (
	"bytes"
	"context"
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

			// Post counts come only from the main site.
			var posts int64
			for _, pt := range inv.PostTypes {
				posts += pt.Count
			}
			if posts != int64(stats["wp_posts"]) {
				t.Errorf("post types add up to %d posts, want %d", posts, stats["wp_posts"])
			}
			pt := findType(inv, "post")
			if pt == nil || pt.Statuses["publish"] == 0 || pt.Statuses["pitch"] == 0 || pt.FirstDate == nil {
				t.Errorf("post type 'post' = %+v", pt)
			}
			if findType(inv, "acf-field") == nil || findType(inv, "revision") == nil {
				t.Error("acf-field or revision post type missing")
			}

			if inv.ACFFields["gallery"] != 1 || inv.ACFFields["post_object"] != 1 {
				t.Errorf("ACF fields = %v", inv.ACFFields)
			}

			tax := map[string]Taxonomy{}
			for _, tx := range inv.Taxonomies {
				tax[tx.Taxonomy] = tx
			}
			if c := tax["category"]; c.Terms != 12 || !c.Hierarchical || c.PostTypes["post"] == 0 || len(c.LargestTerms) != 5 {
				t.Errorf("category = %+v", c)
			}
			if tg := tax["post_tag"]; tg.Terms != 40 || tg.Hierarchical {
				t.Errorf("post_tag = %+v", tg)
			}

			if inv.Options.PostsPerPage == nil || *inv.Options.PostsPerPage != 10 {
				t.Errorf("posts_per_page = %v", inv.Options.PostsPerPage)
			}
			if len(inv.Options.Large) != 1 || inv.Options.Large[0].Name != "big_option" {
				t.Errorf("large options = %v", inv.Options.Large)
			}
			if inv.Options.TransientCount != 2 {
				t.Errorf("transients = %d", inv.Options.TransientCount)
			}
			if _, ok := inv.Options.Values["admin_email"]; ok {
				t.Error("index stored admin_email; option values outside the allow list must not be stored")
			}

			if inv.Users.Count != 5 || inv.Users.Roles["administrator"] != 1 || inv.Users.Roles["author"] != 4 {
				t.Errorf("users = %+v", inv.Users)
			}
			if inv.Comments.Count != int64(stats["wp_comments"]) || inv.Comments.ByStatus["approved"] != inv.Comments.Count {
				t.Errorf("comments = %+v", inv.Comments)
			}

			content := map[string]RefCount{}
			for _, rc := range inv.References.Content {
				content[rc.Name] = rc
			}
			for _, src := range []string{"block:core/image:id", "class:wp-image"} {
				rc := content[src]
				if rc.Refs == 0 || rc.Resolved != rc.Refs {
					t.Errorf("content refs %s = %+v", src, rc)
				}
			}
			var keys []string
			for _, rc := range inv.References.MetaKeys {
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

func TestIndexTablesHoldOnlyMainSite(t *testing.T) {
	_, stats, path := buildIndex(t, synth.Options{Seed: 23, Posts: 100, Subsite: true})
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, rt := range rowTables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM `+rt.table+` WHERE tbl <> ?`, "wp_"+rt.suffix).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s holds %d rows from other prefixes", rt.table, n)
		}
	}
	var bylines int
	if err := db.QueryRow(`SELECT count(*) FROM object_rows WHERE tbl = 'wp_example_bylines' AND column_name = 'post_id'`).Scan(&bylines); err != nil {
		t.Fatal(err)
	}
	if bylines != stats["wp_example_bylines"] {
		t.Errorf("object_rows has %d bylines rows, want %d", bylines, stats["wp_example_bylines"])
	}
}

func findType(inv *Inventory, typ string) *PostType {
	for i := range inv.PostTypes {
		if inv.PostTypes[i].Type == typ {
			return &inv.PostTypes[i]
		}
	}
	return nil
}
