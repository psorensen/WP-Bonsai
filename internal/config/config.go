// Package config reads a project's bonsai.yml. See SPEC.md, "Configuration
// schema". Anything the file leaves out gets the defaults set here.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/psorensen/WP-Bonsai/internal/localurl"
)

// Modes for a post type.
const (
	ModeLatest  = "latest"
	ModePerTerm = "per_term"
	ModeAll     = "all"
	ModeNone    = "none"
)

// Table actions.
const (
	TableKeep   = "keep"
	TableEmpty  = "empty"
	TableFilter = "filter"
)

// Defaults from SPEC.md.
const (
	DefaultTargetSizeMB     = 10
	DefaultDependencyDepth  = 2
	DefaultPruneFlatTermsAt = 500
)

// Config is one project's configuration.
type Config struct {
	Project         string               `yaml:"project"`
	TargetSizeMB    float64              `yaml:"target_size_mb"`
	DependencyDepth *int                 `yaml:"dependency_depth"`
	DefaultPostType *PostType            `yaml:"default_post_type"`
	PostTypes       map[string]*PostType `yaml:"post_types"`
	Taxonomies      Taxonomies           `yaml:"taxonomies"`
	Users           Users                `yaml:"users"`
	Meta            Meta                 `yaml:"meta"`
	Options         Options              `yaml:"options"`
	Local           Local                `yaml:"local"`
	Tables          map[string]TableRule `yaml:"tables"`
	References      References           `yaml:"references"`
	Scrub           Scrub                `yaml:"scrub"`
	// Sites holds settings for single sites of a multisite network, keyed
	// by blog ID. The key "*" applies to every site without its own entry.
	Sites map[string]*SiteConfig `yaml:"sites"`
}

// SiteConfig overrides the top-level settings for one site.
type SiteConfig struct {
	// Exclude drops the site: its tables and its network rows.
	Exclude *bool `yaml:"exclude"`
	// DefaultPostType replaces the top-level default_post_type.
	DefaultPostType *PostType `yaml:"default_post_type"`
	// PostTypes replace the top-level settings type by type.
	PostTypes map[string]*PostType `yaml:"post_types"`
	// References adds meta keys to the top-level list.
	References References `yaml:"references"`
}

// Site is the resolved settings of one site.
type Site struct {
	BlogID  int
	Exclude bool
	// ExcludeSet is true when the config sets exclude for this site, either
	// in its own entry or under "*".
	ExcludeSet      bool
	DefaultPostType *PostType
	PostTypes       map[string]*PostType
	ExtraMetaKeys   []string
}

// ForSite resolves the settings of one site: its own entry under sites,
// then the "*" entry, then the top level.
func (c *Config) ForSite(blogID int) *Site {
	s := &Site{BlogID: blogID, DefaultPostType: c.DefaultPostType, PostTypes: map[string]*PostType{}}
	for k, v := range c.PostTypes {
		s.PostTypes[k] = v
	}
	s.ExtraMetaKeys = append(s.ExtraMetaKeys, c.References.ExtraMetaKeys...)
	for _, key := range []string{"*", strconv.Itoa(blogID)} {
		sc := c.Sites[key]
		if sc == nil {
			continue
		}
		if sc.Exclude != nil {
			s.Exclude, s.ExcludeSet = *sc.Exclude, true
		}
		if sc.DefaultPostType != nil {
			s.DefaultPostType = sc.DefaultPostType
		}
		for k, v := range sc.PostTypes {
			s.PostTypes[k] = v
		}
		s.ExtraMetaKeys = append(s.ExtraMetaKeys, sc.References.ExtraMetaKeys...)
	}
	return s
}

// PostTypeFor returns the settings for a post type and whether they came
// from the config or from default_post_type.
func (s *Site) PostTypeFor(typ string) (*PostType, bool) {
	if pt, ok := s.PostTypes[typ]; ok {
		return pt, true
	}
	return s.DefaultPostType, false
}

// SortedPostTypes returns the configured post type names in order.
func (s *Site) SortedPostTypes() []string { return sortedKeys(s.PostTypes) }

// PostType says which posts of one type to keep as seeds.
type PostType struct {
	Mode string `yaml:"mode"`
	// Count is the number of posts for latest mode.
	Count int `yaml:"count"`
	// PerTerm is the number of posts per term for per_term mode.
	PerTerm    int                     `yaml:"per_term"`
	Taxonomies map[string]TaxonomyPick `yaml:"taxonomies"`
	// Statuses maps a post status to "all" or a number. The mode selects
	// among posts whose status is set to "all". A number adds that many of
	// the newest posts with that status. The default is {publish: all}.
	Statuses map[string]Amount `yaml:"statuses"`
}

// TaxonomyPick limits the terms per_term mode samples from.
type TaxonomyPick struct {
	MaxTerms Amount `yaml:"max_terms"`
	// IncludeMenuTerms adds the terms that the site's menus link to, on top
	// of the first MaxTerms. Those are the archives people click. The
	// default is true.
	IncludeMenuTerms *bool `yaml:"include_menu_terms"`
	// Order picks which terms count as the first MaxTerms: most_used (the
	// default) or name.
	Order string `yaml:"order"`
}

type Taxonomies struct {
	// PruneUnusedFlatTermsOver: a flat taxonomy with more terms than this
	// keeps only the terms that kept posts use.
	PruneUnusedFlatTermsOver *int `yaml:"prune_unused_flat_terms_over"`
}

type Users struct {
	IncludeRoles []string `yaml:"include_roles"`
}

// Local rewrites production URLs for a local environment. With no URL, the
// output keeps its production URLs.
type Local struct {
	// URL is the main site's local address, such as https://news.local.
	URL string `yaml:"url"`
	// Sites sets the local address of other sites of a network, by blog
	// ID. Sites not listed get one derived from their production address.
	Sites map[string]string `yaml:"sites"`
}

// Options filters the options table of every kept site.
type Options struct {
	// Exclude are option names to drop, in addition to transients and
	// Jetpack sync queues. A * matches any run of characters.
	Exclude []string `yaml:"exclude"`
}

type Meta struct {
	// ExcludeKeys are post meta keys to drop from kept posts. Empty by
	// default: every meta row of a kept post is kept. A * matches any run
	// of characters.
	ExcludeKeys []string `yaml:"exclude_keys"`
}

type References struct {
	// ExtraMetaKeys are post meta keys whose values hold post IDs.
	ExtraMetaKeys []string `yaml:"extra_meta_keys"`
}

// Scrub configures the sandbox scrub step. The fueled-default profile
// runs 10up WP Scrubber's "wp scrub all" and then Bonsai's own steps.
type Scrub struct {
	Profile string `yaml:"profile"`
	// AllowedDomains and AllowedEmails are users the scrubber leaves
	// unchanged, in addition to its built-in 10up.com and get10up.com.
	AllowedDomains []string `yaml:"allowed_domains"`
	AllowedEmails  []string `yaml:"allowed_emails"`
}

// Amount is "all" or a non-negative number.
type Amount struct {
	All bool
	N   int
	Set bool // the config file gave a value
}

// UnmarshalYAML reads "all" or a number.
func (a *Amount) UnmarshalYAML(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"'`)
	if s == "all" {
		*a = Amount{All: true, Set: true}
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf(`must be "all" or a number of zero or more, not %s`, s)
	}
	*a = Amount{N: n, Set: true}
	return nil
}

func (a Amount) String() string {
	if a.All {
		return "all"
	}
	return strconv.Itoa(a.N)
}

// TableRule says what to do with a table's rows.
type TableRule struct {
	Action   string // keep, empty, or filter
	FilterBy string // the column holding a post ID, for filter
}

// UnmarshalYAML reads "keep", "empty", or {filter_by: column}.
func (r *TableRule) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err == nil {
		switch s {
		case TableKeep, TableEmpty:
			*r = TableRule{Action: s}
			return nil
		}
		return fmt.Errorf(`must be "keep", "empty", or {filter_by: <column>}, not %q`, s)
	}
	var m struct {
		FilterBy string `yaml:"filter_by"`
	}
	if err := yaml.UnmarshalWithOptions(b, &m, yaml.DisallowUnknownField()); err != nil || m.FilterBy == "" {
		return errors.New(`must be "keep", "empty", or {filter_by: <column>}`)
	}
	*r = TableRule{Action: TableFilter, FilterBy: m.FilterBy}
	return nil
}

// Load reads and checks a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse reads and checks a config, and fills in defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("config: %s", yaml.FormatError(err, false, true))
	}
	c.fillDefaults()
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Default returns the config used when a project has no bonsai.yml.
func Default() *Config {
	c := &Config{}
	c.fillDefaults()
	return c
}

func intPtr(n int) *int { return &n }

func intBool(b bool) *bool { return &b }

func (c *Config) fillDefaults() {
	if c.TargetSizeMB == 0 {
		c.TargetSizeMB = DefaultTargetSizeMB
	}
	if c.DependencyDepth == nil {
		c.DependencyDepth = intPtr(DefaultDependencyDepth)
	}
	if c.DefaultPostType == nil {
		c.DefaultPostType = &PostType{Mode: ModeLatest, Count: 10}
	}
	if c.PostTypes == nil {
		c.PostTypes = map[string]*PostType{}
	}
	fill := func(pt *PostType) {
		if pt == nil {
			return
		}
		if len(pt.Statuses) == 0 {
			pt.Statuses = map[string]Amount{"publish": {All: true}}
		}
		for name, tp := range pt.Taxonomies {
			if tp.Order == "" {
				tp.Order = "most_used"
			}
			if tp.IncludeMenuTerms == nil {
				tp.IncludeMenuTerms = intBool(true)
			}
			// An unset max_terms means every term. An explicit 0 means
			// only the terms the menus link to.
			if !tp.MaxTerms.Set {
				tp.MaxTerms = Amount{All: true}
			}
			pt.Taxonomies[name] = tp
		}
	}
	fill(c.DefaultPostType)
	for _, pt := range c.PostTypes {
		fill(pt)
	}
	for _, sc := range c.Sites {
		if sc == nil {
			continue
		}
		fill(sc.DefaultPostType)
		for _, pt := range sc.PostTypes {
			fill(pt)
		}
	}
	if c.Taxonomies.PruneUnusedFlatTermsOver == nil {
		c.Taxonomies.PruneUnusedFlatTermsOver = intPtr(DefaultPruneFlatTermsAt)
	}
	if c.Users.IncludeRoles == nil {
		c.Users.IncludeRoles = []string{"administrator"}
	}
	if c.Scrub.Profile == "" {
		c.Scrub.Profile = "fueled-default"
	}
}

func (c *Config) check() error {
	var errs []string
	bad := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.TargetSizeMB < 0 {
		bad("target_size_mb: must be more than 0")
	}
	if *c.DependencyDepth < 0 {
		bad("dependency_depth: must be 0 or more")
	}
	checkType := func(path string, pt *PostType) {
		if pt == nil {
			bad("%s: needs a mode", path)
			return
		}
		switch pt.Mode {
		case ModeLatest:
			if pt.Count <= 0 {
				bad("%s.count: latest mode needs a count above 0", path)
			}
		case ModePerTerm:
			if pt.PerTerm <= 0 {
				bad("%s.per_term: per_term mode needs a number above 0", path)
			}
			if len(pt.Taxonomies) == 0 {
				bad("%s.taxonomies: per_term mode needs at least one taxonomy", path)
			}
			for name, tp := range pt.Taxonomies {
				if tp.Order != "most_used" && tp.Order != "name" {
					bad("%s.taxonomies.%s.order: must be most_used or name", path, name)
				}
			}
		case ModeAll, ModeNone:
		case "":
			bad("%s.mode: missing; use latest, per_term, all, or none", path)
		default:
			bad("%s.mode: %q is not latest, per_term, all, or none", path, pt.Mode)
		}
		if pt.Mode != ModeLatest && pt.Count != 0 {
			bad("%s.count: only latest mode takes a count", path)
		}
		if pt.Mode != ModePerTerm && (pt.PerTerm != 0 || len(pt.Taxonomies) > 0) {
			bad("%s: only per_term mode takes per_term and taxonomies", path)
		}
	}
	checkTypes := func(path string, def *PostType, types map[string]*PostType) {
		if def != nil {
			checkType(path+"default_post_type", def)
		}
		for _, name := range sortedKeys(types) {
			if why, fixed := FixedPostTypes[name]; fixed {
				bad("%spost_types.%s: this type is always %s and cannot be configured", path, name, why)
				continue
			}
			checkType(path+"post_types."+name, types[name])
		}
	}
	checkTypes("", c.DefaultPostType, c.PostTypes)
	for _, key := range sortedKeys(c.Sites) {
		path := fmt.Sprintf("sites.%s.", key)
		if n, err := strconv.Atoi(key); key != "*" && (err != nil || n < 1) {
			bad("sites.%s: the key must be a blog ID or \"*\"", key)
			continue
		}
		sc := c.Sites[key]
		if sc == nil {
			continue
		}
		if key == "1" && sc.Exclude != nil && *sc.Exclude {
			bad("sites.1.exclude: the main site cannot be excluded")
		}
		checkTypes(path, sc.DefaultPostType, sc.PostTypes)
	}
	if c.Scrub.Profile != "fueled-default" {
		bad("scrub.profile: %q is not a known profile; use fueled-default", c.Scrub.Profile)
	}
	if c.Local.URL != "" {
		if _, err := localurl.Parse(c.Local.URL); err != nil {
			bad("local.url: %v", err)
		}
	} else if len(c.Local.Sites) > 0 {
		bad("local.sites needs local.url")
	}
	for _, key := range sortedKeys(c.Local.Sites) {
		if n, err := strconv.Atoi(key); err != nil || n < 2 {
			bad("local.sites.%s: the key must be the blog ID of a subsite", key)
		} else if _, err := localurl.Parse(c.Local.Sites[key]); err != nil {
			bad("local.sites.%s: %v", key, err)
		}
	}
	if *c.Taxonomies.PruneUnusedFlatTermsOver < 0 {
		bad("taxonomies.prune_unused_flat_terms_over: must be 0 or more")
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "\nconfig: "))
	}
	return nil
}

// FixedPostTypes are always kept in full or always dropped, as SPEC.md lists
// them, plus types that hold personal data.
var FixedPostTypes = map[string]string{
	"nav_menu_item":    "kept",
	"wp_navigation":    "kept",
	"wp_template":      "kept",
	"wp_template_part": "kept",
	"wp_global_styles": "kept",
	"wp_block":         "kept",
	"wp_font_family":   "kept",
	"wp_font_face":     "kept",
	"custom_css":       "kept",
	"acf-field-group":  "kept",
	"acf-field":        "kept",
	"acf-post-type":    "kept",
	"acf-taxonomy":     "kept",

	"revision":            "dropped",
	"customize_changeset": "dropped",
	"oembed_cache":        "dropped",
	// Personal data: orders, privacy requests, form submissions.
	"shop_order":           "dropped",
	"shop_order_refund":    "dropped",
	"shop_order_placehold": "dropped",
	"shop_subscription":    "dropped",
	"user_request":         "dropped",
	"flamingo_inbound":     "dropped",
	"flamingo_contact":     "dropped",
	"scheduled-action":     "dropped",
}

// FixedTypes returns the post types that are always kept or always dropped.
func FixedTypes(why string) []string {
	var out []string
	for t, w := range FixedPostTypes {
		if w == why {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// LocalOverrides returns local.sites by blog ID.
func (c *Config) LocalOverrides() map[int]string {
	out := map[int]string{}
	for k, v := range c.Local.Sites {
		if n, err := strconv.Atoi(k); err == nil {
			out[n] = v
		}
	}
	return out
}
