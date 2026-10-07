package config

import (
	"strings"
	"testing"
)

// specExample is the example config from SPEC.md.
const specExample = `
project: example-newspaper
target_size_mb: 10
dependency_depth: 2

post_types:
  post:
    mode: per_term
    per_term: 20
    taxonomies:
      category: { max_terms: all }
      post_tag: { max_terms: 50, order: most_used }
    statuses: { publish: all, draft: 3, future: 2, pitch: 2 }
  page:
    mode: all
  product:
    mode: latest
    count: 100
  obituary:
    mode: latest
    count: 25
  ad_campaign:
    mode: none

taxonomies:
  prune_unused_flat_terms_over: 500

comments:
  per_post: 3

users:
  include_roles: [administrator]

meta:
  exclude_keys: ["_edit_lock", "_edit_last", "_oembed_*", "legacy_import_*"]

tables:
  wp_custom_paywall_log: empty
  wp_custom_bylines: { filter_by: post_id }
  wp_custom_settings: keep

references:
  extra_meta_keys: [related_story_id, hero_video_id]

scrub:
  profile: fueled-default
`

func TestParseSpecExample(t *testing.T) {
	c, err := Parse([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	post := c.PostTypes["post"]
	if post.Mode != ModePerTerm || post.PerTerm != 20 {
		t.Errorf("post = %+v", post)
	}
	if tp := post.Taxonomies["category"]; !tp.MaxTerms.All || tp.Order != "most_used" {
		t.Errorf("category pick = %+v", tp)
	}
	if tp := post.Taxonomies["post_tag"]; tp.MaxTerms.N != 50 {
		t.Errorf("post_tag pick = %+v", tp)
	}
	if s := post.Statuses; !s["publish"].All || s["draft"].N != 3 || s["pitch"].N != 2 {
		t.Errorf("statuses = %+v", s)
	}
	if s := c.PostTypes["page"].Statuses; len(s) != 1 || !s["publish"].All {
		t.Errorf("page statuses default = %+v", s)
	}
	if r := c.Tables["wp_custom_bylines"]; r.Action != TableFilter || r.FilterBy != "post_id" {
		t.Errorf("bylines rule = %+v", r)
	}
	if r := c.Tables["wp_custom_paywall_log"]; r.Action != TableEmpty {
		t.Errorf("paywall log rule = %+v", r)
	}
	if *c.DependencyDepth != 2 || *c.Comments.PerPost != 3 || c.References.ExtraMetaKeys[1] != "hero_video_id" {
		t.Errorf("config = %+v", c)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("project: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TargetSizeMB != 10 || *c.DependencyDepth != 2 || *c.Comments.PerPost != 3 ||
		*c.Taxonomies.PruneUnusedFlatTermsOver != 500 || c.Users.IncludeRoles[0] != "administrator" {
		t.Errorf("defaults = %+v", c)
	}
	pt, fromConfig := c.ForSite(1).PostTypeFor("anything")
	if fromConfig || pt.Mode != ModeLatest || pt.Count != 10 {
		t.Errorf("default post type = %+v", pt)
	}
	zero, err := Parse([]byte("dependency_depth: 0\ncomments: {per_post: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if *zero.DependencyDepth != 0 || *zero.Comments.PerPost != 0 {
		t.Error("an explicit 0 was replaced by the default")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"post_types: {post: {mode: newest}}":                                      `"newest" is not`,
		"post_types: {post: {mode: latest}}":                                      "needs a count",
		"post_types: {post: {mode: per_term, per_term: 5}}":                       "at least one taxonomy",
		"post_types: {post: {mode: all, count: 5}}":                               "only latest mode",
		"post_types: {revision: {mode: all}}":                                     "always dropped",
		"post_types: {post: {mode: latest, count: 1, statuses: {publish: some}}}": `"all" or a number`,
		"tables: {wp_x: delete}":                                                  `"keep", "empty"`,
		"tables: {wp_x: {filter: post_id}}":                                       "filter_by",
		"unknown_key: 1":                                                          "unknown_key",
		"dependency_depth: -1":                                                    "0 or more",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error containing %q", in, err, want)
		}
	}
}

func TestSites(t *testing.T) {
	c, err := Parse([]byte(`
post_types:
  post: { mode: latest, count: 5 }
  page: { mode: all }
references:
  extra_meta_keys: [hero_id]
sites:
  "*":
    exclude: true
    post_types:
      post: { mode: latest, count: 2 }
  "1": { exclude: false }
  "30":
    exclude: false
    default_post_type: { mode: none }
    post_types:
      page: { mode: none }
    references:
      extra_meta_keys: [sports_hero_id]
`))
	if err != nil {
		t.Fatal(err)
	}
	one, thirty, two := c.ForSite(1), c.ForSite(30), c.ForSite(2)
	if one.Exclude || thirty.Exclude || !two.Exclude {
		t.Errorf("exclude: site 1 %v, 30 %v, 2 %v", one.Exclude, thirty.Exclude, two.Exclude)
	}
	if one.PostTypes["post"].Count != 2 || one.PostTypes["page"].Mode != ModeAll {
		t.Errorf("site 1 post types = %+v %+v", one.PostTypes["post"], one.PostTypes["page"])
	}
	if thirty.PostTypes["page"].Mode != ModeNone || thirty.DefaultPostType.Mode != ModeNone {
		t.Errorf("site 30 = %+v", thirty)
	}
	if got := strings.Join(thirty.ExtraMetaKeys, ","); got != "hero_id,sports_hero_id" {
		t.Errorf("site 30 extra keys = %s", got)
	}
	if c.PostTypes["post"].Count != 5 {
		t.Error("ForSite changed the top-level config")
	}

	for in, want := range map[string]string{
		"sites: {main: {}}":                                     "blog ID",
		"sites: {\"1\": {exclude: true}}":                       "cannot be excluded",
		"sites: {\"2\": {post_types: {revision: {mode: all}}}}": "sites.2.post_types.revision",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error containing %q", in, err, want)
		}
	}
}
