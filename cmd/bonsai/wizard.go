package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/localurl"
	"github.com/psorensen/WP-Bonsai/internal/plan"
)

// Interactive mode: "bonsai <dump>". It indexes the dump, asks a few
// questions with defaults sized from the dump, saves bonsai.yml, and runs
// the full build.

const (
	wizardConfigFile = "bonsai.yml"
	wizardWorkDir    = ".bonsai-work"
	defaultTargetMB  = 50
	defaultMaxTerms  = 10
	// maxDefaultMenuTerms is the most menu-linked terms the wizard fills
	// by default.
	maxDefaultMenuTerms = 20
)

// typeChoice is one post type the wizard asks about.
type typeChoice struct {
	Type      string
	Published int64
	Taxonomy  string // per-term taxonomy, or "" for latest mode
	Answer    string // "all", "0", or a number; empty means Default
	Default   string
	// For per-term types: how many of the most-used terms get full
	// archives, besides the terms the menus link to.
	Terms        string
	TermsDefault string
	// MenuTerms is the most terms one kept site's menus link to.
	// IncludeMenu says whether those terms also get full archives.
	MenuTerms   int64
	IncludeMenu bool
}

// terms returns the terms answer, or its default when it is blank.
func (c typeChoice) terms() string {
	if a := strings.TrimSpace(strings.ToLower(c.Terms)); a != "" {
		return a
	}
	return c.TermsDefault
}

// answer returns the user's answer, or the default when it is blank.
func (c typeChoice) answer() string {
	if a := strings.TrimSpace(strings.ToLower(c.Answer)); a != "" {
		return a
	}
	return c.Default
}

func wizardCmd(ctx context.Context, dumpPath string) error {
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return errors.New("interactive mode needs a terminal; for scripts, run: bonsai build <dump> -config bonsai.yml")
	}
	if _, err := os.Stat(dumpPath); err != nil {
		return err
	}
	if err := setupSandbox(ctx); err != nil {
		return err
	}

	out := outputName(dumpPath)
	fmt.Printf("Bonsai will shrink %s into %s.\n\n", dumpPath, out)

	// Reuse an existing config if the user wants to.
	if _, err := os.Stat(wizardConfigFile); err == nil {
		reuse := true
		if err := ask(huh.NewConfirm().
			Title("Use the bonsai.yml in this folder?").
			Description("Choose No to answer the questions again. Your answers replace bonsai.yml.").
			Value(&reuse)); err != nil {
			return err
		}
		if reuse {
			cfg, err := config.Load(wizardConfigFile)
			if err != nil {
				return err
			}
			if cfg.Local.URL == "" {
				if cfg, err = addLocalToConfig(ctx, dumpPath, cfg); err != nil {
					return err
				}
			}
			return runBuild(ctx, buildRun{dump: dumpPath, cfg: cfg, cfgName: wizardConfigFile, work: wizardWorkDir, out: out})
		}
	}

	indexPath, err := ensureIndex(ctx, dumpPath, wizardWorkDir)
	if err != nil {
		return err
	}
	db, err := index.Open(indexPath)
	if err != nil {
		return err
	}
	inv, err := index.ReadInventory(ctx, db)
	db.Close()
	if err != nil {
		return err
	}
	fmt.Printf("\nThe dump holds %d site(s), %d tables, and %s of SQL.\n\n", len(inv.Sites), len(inv.Tables), plan.FormatBytes(inv.Source.Bytes))

	// Sites.
	kept, err := askSites(inv)
	if err != nil {
		return err
	}

	// Target size.
	target := strconv.Itoa(defaultTargetMB)
	if err := ask(huh.NewInput().
		Title(fmt.Sprintf("Target size of the output, in MB (Enter keeps %d)", defaultTargetMB)).
		Description("Bonsai warns when the result is bigger. It never cuts the result to fit.").
		Value(&target).
		Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return nil
			}
			if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil || v <= 0 {
				return errors.New("enter a number above 0")
			}
			return nil
		})); err != nil {
		return err
	}
	targetMB, err := strconv.ParseFloat(strings.TrimSpace(target), 64)
	if err != nil {
		targetMB = defaultTargetMB
	}

	localURL, localSites, err := askLocal(inv, kept)
	if err != nil {
		return err
	}

	choices := suggestTypes(inv, kept)
	for {
		if err := askTypes(choices); err != nil {
			return err
		}
		text := wizardYAML(dumpPath, targetMB, choices, inv, kept) + localYAML(localURL, localSites)
		cfg, err := config.Parse([]byte(text))
		if err != nil {
			return fmt.Errorf("bonsai wrote a config it cannot read; please report this: %w", err)
		}

		p, err := plan.Build(ctx, indexPath, cfg)
		if err != nil {
			return err
		}
		printEstimate(p)
		over := p.EstimateBytes > p.TargetBytes
		p.Close()

		// Over the target, nudge toward changing the numbers.
		next := "build"
		if over {
			next = "change"
		}
		if err := ask(huh.NewSelect[string]().
			Title("What next?").
			Options(
				huh.NewOption("Build it", "build"),
				huh.NewOption("Change the numbers", "change"),
				huh.NewOption("Save bonsai.yml and stop", "save"),
			).Value(&next)); err != nil {
			return err
		}
		if next == "change" {
			continue
		}
		if err := os.WriteFile(wizardConfigFile, []byte(text), 0o644); err != nil {
			return err
		}
		fmt.Printf("Saved %s. Next time, bonsai %s offers to reuse it.\n\n", wizardConfigFile, filepath.Base(dumpPath))
		if next == "save" {
			return nil
		}
		return runBuild(ctx, buildRun{dump: dumpPath, cfg: cfg, cfgName: wizardConfigFile, work: wizardWorkDir, out: out})
	}
}

// outputName returns the output file for a dump: prod.sql.gz becomes
// prod-bonsai.sql in the current folder.
func outputName(dumpPath string) string {
	base := filepath.Base(dumpPath)
	for _, ext := range []string{".gz", ".sql"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base + "-bonsai.sql"
}

// askSites asks which subsites of a network to keep. The main site is
// always kept. It returns the kept blog IDs.
func askSites(inv *index.Inventory) (map[int]bool, error) {
	kept := map[int]bool{}
	var opts []huh.Option[int]
	for _, s := range inv.Sites {
		if s.BlogID == 1 {
			kept[1] = true
			continue
		}
		if !listedSite(s) {
			continue // left over from a deleted site; the plan drops it
		}
		label := fmt.Sprintf("%d  %s%s  (%s, %d posts)", s.BlogID, s.Domain, s.Path, plan.FormatBytes(s.Bytes), sitePosts(s))
		opts = append(opts, huh.NewOption(label, s.BlogID).Selected(true))
	}
	if len(opts) == 0 {
		return kept, nil
	}
	var picked []int
	if err := ask(huh.NewMultiSelect[int]().
		Title("Which sites should the dump keep?").
		Description("The main site is always kept. Space toggles a site, Enter confirms.").
		Options(opts...).
		Value(&picked)); err != nil {
		return nil, err
	}
	for _, id := range picked {
		kept[id] = true
	}
	return kept, nil
}

// listedSite reports whether a site has a row in wp_blogs.
func listedSite(s index.Site) bool { return s.Domain != "" }

func sitePosts(s index.Site) int64 {
	var n int64
	for _, pt := range s.PostTypes {
		n += pt.Count
	}
	return n
}

// suggestTypes picks a default for every post type with published posts on
// a kept site. Big types with a category taxonomy keep the newest posts of
// each category, one more than posts_per_page, so archives have a page 2.
// Small types are kept whole. Everything else keeps its 20 newest posts.
func suggestTypes(inv *index.Inventory, kept map[int]bool) []typeChoice {
	published := map[string]int64{}
	taxRel := map[string]map[string]int64{} // type -> taxonomy -> relationships
	hier := map[string]bool{}
	menuTerms := map[string]int64{} // taxonomy -> most menu-linked terms on one site
	ppp := int64(10)
	for _, s := range inv.Sites {
		if !kept[s.BlogID] {
			continue
		}
		for _, pt := range s.PostTypes {
			if _, fixed := config.FixedPostTypes[pt.Type]; fixed || pt.Type == "attachment" {
				continue
			}
			published[pt.Type] += pt.Statuses["publish"]
		}
		for _, tx := range s.Taxonomies {
			hier[tx.Taxonomy] = hier[tx.Taxonomy] || tx.Hierarchical
			menuTerms[tx.Taxonomy] = max(menuTerms[tx.Taxonomy], tx.MenuTerms)
			for typ, n := range tx.PostTypes {
				if taxRel[typ] == nil {
					taxRel[typ] = map[string]int64{}
				}
				taxRel[typ][tx.Taxonomy] += n
			}
		}
		if s.Options.PostsPerPage != nil && *s.Options.PostsPerPage > ppp {
			ppp = *s.Options.PostsPerPage
		}
	}

	var out []typeChoice
	for typ, n := range published {
		if n == 0 {
			continue
		}
		c := typeChoice{Type: typ, Published: n}
		switch {
		case n <= 50:
			c.Answer = "all"
		default:
			// Only the main post type gets per-term archives. Giving every
			// type with categories its own posts per term multiplies the
			// result for little gain.
			if typ == "post" {
				c.Taxonomy = categoryTaxonomy(taxRel[typ], hier)
			}
			if c.Taxonomy != "" && n > 200 {
				c.Answer = strconv.FormatInt(ppp+1, 10)
				c.TermsDefault = strconv.Itoa(defaultMaxTerms)
				c.Terms = c.TermsDefault
				// Menu terms are the archives people click, but some menus
				// link to hundreds of terms. Fill them by default only
				// when the menus are small.
				c.MenuTerms = menuTerms[c.Taxonomy]
				c.IncludeMenu = c.MenuTerms <= maxDefaultMenuTerms
			} else {
				c.Taxonomy = ""
				c.Answer = "20"
			}
		}
		c.Default = c.Answer
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Published != out[j].Published {
			return out[i].Published > out[j].Published
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// categoryTaxonomy picks the taxonomy whose archives matter for a post type:
// "category" if the type uses it, else the hierarchical taxonomy with
// "categor" in its name that the type uses most.
func categoryTaxonomy(rel map[string]int64, hier map[string]bool) string {
	if rel["category"] > 0 {
		return "category"
	}
	best, bestN := "", int64(0)
	for tax, n := range rel {
		if hier[tax] && strings.Contains(tax, "categor") && n > bestN {
			best, bestN = tax, n
		}
	}
	return best
}

// askTypes asks for one number per post type.
func askTypes(choices []typeChoice) error {
	var fields []huh.Field
	for i := range choices {
		c := &choices[i]
		title := fmt.Sprintf("%s: %s published (Enter keeps %s)", c.Type, humanInt(c.Published), c.Default)
		desc := "How many of the newest posts to keep: a number, all, or 0."
		if c.Taxonomy != "" {
			desc = fmt.Sprintf("How many of the newest posts to keep in each %s: a number, all, or 0.", c.Taxonomy)
		}
		fields = append(fields, huh.NewInput().Title(title).Description(desc).Value(&c.Answer).Validate(validAnswer))
		if c.Taxonomy != "" {
			fields = append(fields, huh.NewInput().
				Title(fmt.Sprintf("%s: how many of the most-used %s terms get those posts? (Enter keeps %s)", c.Type, c.Taxonomy, c.TermsDefault)).
				Description("Other terms keep their archive pages, with fewer posts. A number, all, or 0.").
				Value(&c.Terms).Validate(validAnswer))
			if c.MenuTerms > 0 {
				fields = append(fields, huh.NewConfirm().
					Title(fmt.Sprintf("%s: also give those posts to the %s terms your menus link to? (up to %d on one site)", c.Type, c.Taxonomy, c.MenuTerms)).
					Description("These are the archives people click. Large menus add many posts.").
					Value(&c.IncludeMenu))
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return huh.NewForm(huh.NewGroup(fields...).
		Title("Post types").
		Description("Bonsai also keeps everything the kept posts depend on: images, parent pages, menus, and related posts.")).
		WithAccessible(accessible()).Run()
}

func validAnswer(s string) error {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "all" || s == "" {
		return nil
	}
	if n, err := strconv.Atoi(s); err != nil || n < 0 {
		return errors.New("enter a number, all, or 0")
	}
	return nil
}

// wizardYAML writes the answers as a bonsai.yml.
func wizardYAML(dumpPath string, targetMB float64, choices []typeChoice, inv *index.Inventory, kept map[int]bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by bonsai on %s for %s.\n", time.Now().Format(time.DateOnly), filepath.Base(dumpPath))
	b.WriteString("# Edit it freely. See the README for every option.\n\n")
	fmt.Fprintf(&b, "target_size_mb: %s\n", strconv.FormatFloat(targetMB, 'f', -1, 64))

	if len(choices) > 0 {
		b.WriteString("\npost_types:\n")
		for _, c := range choices {
			ans := c.answer()
			fmt.Fprintf(&b, "  %s:\n", yamlKey(c.Type))
			switch {
			case ans == "0":
				b.WriteString("    mode: none\n")
			case ans == "all":
				b.WriteString("    mode: all\n")
			case c.Taxonomy != "":
				fmt.Fprintf(&b, "    mode: per_term\n    per_term: %s\n    taxonomies:\n      %s: { max_terms: %s, include_menu_terms: %t }\n", ans, yamlKey(c.Taxonomy), c.terms(), c.IncludeMenu)
			default:
				fmt.Fprintf(&b, "    mode: latest\n    count: %s\n", ans)
			}
		}
	}

	var excluded []int
	for _, s := range inv.Sites {
		if s.BlogID != 1 && listedSite(s) && !kept[s.BlogID] {
			excluded = append(excluded, s.BlogID)
		}
	}
	if len(excluded) > 0 {
		slices.Sort(excluded)
		b.WriteString("\nsites:\n")
		for _, id := range excluded {
			fmt.Fprintf(&b, "  \"%d\": { exclude: true }\n", id)
		}
	}
	return b.String()
}

// yamlKey quotes a key when it is not a plain word.
func yamlKey(s string) string {
	for _, r := range s {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return strconv.Quote(s)
		}
	}
	return s
}

func printEstimate(p *plan.Plan) {
	fmt.Println()
	for _, s := range p.Sites {
		if s.Excluded {
			continue
		}
		fmt.Printf("  site %-3d %-40s %7s posts kept, %s\n", s.BlogID, s.Domain+s.Path, humanInt(s.KeptPosts), plan.FormatBytes(s.Bytes))
	}
	status := "under"
	if p.EstimateBytes > p.TargetBytes {
		status = "over"
	}
	fmt.Printf("\nEstimated size: %s, %s the target of %s.\n\n", plan.FormatBytes(p.EstimateBytes), status, plan.FormatBytes(p.TargetBytes))
	if status == "over" {
		top := slices.Clone(p.Tables)
		slices.SortFunc(top, func(a, b plan.TablePlan) int { return int(b.Bytes - a.Bytes) })
		fmt.Println("The biggest tables in the result:")
		for _, t := range top[:min(5, len(top))] {
			fmt.Printf("  %-40s %10s  %s rows kept\n", t.Name, plan.FormatBytes(t.Bytes), humanInt(t.KeptRows))
		}
		fmt.Println("\nKeep fewer posts to shrink posts and post meta. For other tables, add a rule to bonsai.yml.")
		fmt.Println()
	}
}

// humanInt formats 830374 as 830,374.
func humanInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ask runs one prompt. With BONSAI_ACCESSIBLE=1 it asks plain line-by-line
// questions instead of drawing on the screen, for screen readers.
func ask(f huh.Field) error {
	return huh.NewForm(huh.NewGroup(f)).WithAccessible(accessible()).Run()
}

func accessible() bool { return os.Getenv("BONSAI_ACCESSIBLE") == "1" }

// prodSites lists the production address of every kept site, for the local
// URL rules. A single site has no wp_blogs row, so its home option is used.
func prodSites(inv *index.Inventory, kept map[int]bool) []localurl.Site {
	var out []localurl.Site
	for _, s := range inv.Sites {
		if !kept[s.BlogID] {
			continue
		}
		site := localurl.Site{BlogID: s.BlogID, Domain: s.Domain, Path: s.Path}
		if site.Domain == "" {
			if u, err := url.Parse(s.Options.Values["home"]); err == nil && u.Host != "" {
				site.Domain, site.Path = u.Host, u.Path+"/"
			}
		}
		if site.Domain != "" {
			out = append(out, site)
		}
	}
	return out
}

// askLocal asks for the local URL of the main site and, on a network,
// shows every site's local address. It returns the URL, empty to keep
// production URLs, and any addresses the user changed.
func askLocal(inv *index.Inventory, kept map[int]bool) (string, map[int]string, error) {
	sites := prodSites(inv, kept)
	if len(sites) == 0 {
		return "", nil, nil
	}
	suggested := localurl.Suggest(sites[0].Domain)
	answer := suggested
	if err := ask(huh.NewInput().
		Title(fmt.Sprintf("Local URL of the site (Enter keeps %s)", suggested)).
		Description("Bonsai rewrites production URLs to local ones, so the dump works in your local environment. Type none to keep production URLs.").
		Value(&answer).
		Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if s == "" || strings.EqualFold(s, "none") {
				return nil
			}
			_, err := localurl.Parse(s)
			return err
		})); err != nil {
		return "", nil, err
	}
	answer = strings.TrimSpace(answer)
	switch {
	case strings.EqualFold(answer, "none"):
		return "", nil, nil
	case answer == "":
		answer = suggested
	}
	base, _ := localurl.Parse(answer)
	if len(sites) == 1 {
		return base, nil, nil
	}

	targets, err := localurl.Targets(base, sites, nil)
	if err != nil {
		return "", nil, err
	}
	fmt.Println("\nLocal addresses:")
	for i, t := range targets {
		fmt.Printf("  site %-3d %-40s -> %s\n", t.BlogID, sites[i].Domain+sites[i].Path, t.URL)
	}
	fmt.Println()
	useThem := true
	if err := ask(huh.NewConfirm().Title("Use these local addresses?").
		Description("Choose No to change them one by one.").Value(&useThem)); err != nil {
		return "", nil, err
	}
	if useThem {
		return base, nil, nil
	}

	answers := make([]string, len(targets))
	var fields []huh.Field
	for i, t := range targets {
		if t.BlogID == 1 {
			continue
		}
		answers[i] = t.URL
		fields = append(fields, huh.NewInput().
			Title(fmt.Sprintf("Site %d, %s%s (Enter keeps %s)", t.BlogID, sites[i].Domain, sites[i].Path, t.URL)).
			Value(&answers[i]).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return nil
				}
				_, err := localurl.Parse(s)
				return err
			}))
	}
	if err := huh.NewForm(huh.NewGroup(fields...).Title("Local addresses")).WithAccessible(accessible()).Run(); err != nil {
		return "", nil, err
	}
	overrides := map[int]string{}
	for i, t := range targets {
		if a := strings.TrimSpace(answers[i]); a != "" && a != t.URL {
			overrides[t.BlogID], _ = localurl.Parse(a)
		}
	}
	if _, err := localurl.Targets(base, sites, overrides); err != nil {
		return "", nil, err
	}
	return base, overrides, nil
}

// localYAML writes the local section of bonsai.yml.
func localYAML(localURL string, sites map[int]string) string {
	if localURL == "" {
		return "\n# No local section: the dump keeps its production URLs.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nlocal:\n  url: %s\n", strconv.Quote(localURL))
	if len(sites) > 0 {
		ids := make([]int, 0, len(sites))
		for id := range sites {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		b.WriteString("  sites:\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "    \"%d\": %s\n", id, strconv.Quote(sites[id]))
		}
	}
	return b.String()
}

// addLocalToConfig asks for the local URL when a reused bonsai.yml has
// none, and appends the answer to the file.
func addLocalToConfig(ctx context.Context, dumpPath string, cfg *config.Config) (*config.Config, error) {
	indexPath, err := ensureIndex(ctx, dumpPath, wizardWorkDir)
	if err != nil {
		return nil, err
	}
	db, err := index.Open(indexPath)
	if err != nil {
		return nil, err
	}
	inv, err := index.ReadInventory(ctx, db)
	db.Close()
	if err != nil {
		return nil, err
	}
	kept := map[int]bool{}
	for _, s := range inv.Sites {
		if !cfg.ForSite(s.BlogID).Exclude {
			kept[s.BlogID] = true
		}
	}
	localURL, sites, err := askLocal(inv, kept)
	if err != nil || localURL == "" {
		return cfg, err
	}
	text, err := os.ReadFile(wizardConfigFile)
	if err != nil {
		return nil, err
	}
	text = append(text, []byte(localYAML(localURL, sites))...)
	if err := os.WriteFile(wizardConfigFile, text, 0o644); err != nil {
		return nil, err
	}
	fmt.Printf("Added the local URL to %s.\n\n", wizardConfigFile)
	return config.Parse(text)
}
