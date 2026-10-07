package finish

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/localurl"
)

// localize rewrites production URLs to the local addresses in the config:
// the network tables, wp-config.php in the sandbox, and every URL in the
// database through WP-CLI search-replace, which handles serialized values.
func (f *finisher) localize() error {
	var prod []localurl.Site
	sources := map[int][]string{}
	homes := map[int]string{}
	for _, s := range f.sites {
		if !f.has(s.Prefix + "options") {
			continue
		}
		out, err := f.sql(fmt.Sprintf("SELECT option_name, option_value FROM %soptions WHERE option_name IN ('home', 'siteurl')", s.Prefix))
		if err != nil {
			return err
		}
		vals := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if k, v, ok := strings.Cut(line, "\t"); ok {
				vals[k] = strings.TrimSuffix(v, "/")
			}
		}
		home := vals["home"]
		homes[s.BlogID] = home
		site := localurl.Site{BlogID: s.BlogID, Domain: s.Domain, Path: s.Path}
		if site.Domain == "" {
			// A single site has no wp_blogs row; its address is home.
			u, err := url.Parse(home)
			if err != nil || u.Host == "" {
				return fmt.Errorf("site %d: cannot read the home option %q", s.BlogID, home)
			}
			site.Domain, site.Path = u.Host, u.Path+"/"
		}
		prod = append(prod, site)
		src := []string{site.Domain + site.Path, home}
		if sv := vals["siteurl"]; sv != "" && !strings.HasPrefix(stripScheme(sv), stripScheme(home)) {
			src = append(src, sv)
		}
		sources[s.BlogID] = src
	}

	targets, err := localurl.Targets(f.cfg.Local.URL, prod, f.cfg.LocalOverrides())
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	byID := map[int]localurl.Target{}
	for _, t := range targets {
		byID[t.BlogID] = t
	}
	main := byID[1]

	if f.multisite {
		var stmts []string
		subdomain := false
		for _, t := range targets {
			stmts = append(stmts, fmt.Sprintf("UPDATE %sblogs SET domain = %s, path = %s WHERE blog_id = %d;",
				f.prefix, sqlString(t.Host), sqlString(t.Path), t.BlogID))
			subdomain = subdomain || t.Host != main.Host
		}
		stmts = append(stmts,
			fmt.Sprintf("UPDATE %ssite SET domain = %s, path = %s WHERE id = 1;", f.prefix, sqlString(main.Host), sqlString(main.Path)),
			fmt.Sprintf("UPDATE %ssitemeta SET meta_value = %s WHERE meta_key = 'siteurl';", f.prefix, sqlString(main.URL+"/")),
			fmt.Sprintf("UPDATE %ssitemeta SET meta_value = %s WHERE meta_key = 'subdomain_install';", f.prefix, sqlString(boolDigit(subdomain))))
		if _, err := f.sql(strings.Join(stmts, "\n")); err != nil {
			return err
		}
		for _, c := range [][3]string{
			{"DOMAIN_CURRENT_SITE", main.Host, ""},
			{"PATH_CURRENT_SITE", main.Path, ""},
			{"SUBDOMAIN_INSTALL", strconv.FormatBool(subdomain), "raw"},
		} {
			args := []string{"config", "set", c[0], c[1]}
			if c[2] == "raw" {
				args = append(args, "--raw")
			}
			if _, err := f.wp(args...); err != nil {
				return err
			}
		}
	}

	pairs, err := localurl.Replacements(targets, sources)
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	var total int64
	for _, r := range pairs {
		for _, form := range [][2]string{
			{"//" + r.From, "//" + r.To},
			{`\/\/` + strings.ReplaceAll(r.From, "/", `\/`), `\/\/` + strings.ReplaceAll(r.To, "/", `\/`)},
		} {
			args := []string{"search-replace", form[0], form[1], "--skip-columns=guid", "--format=count"}
			if f.multisite {
				args = append(args, "--network", "--url="+main.Host+main.Path)
			}
			out, err := f.wp(args...)
			if err != nil {
				return fmt.Errorf("local: search-replace %s: %w", r.From, err)
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(lastLine(out)), 10, 64)
			total += n
		}
	}

	// Set the local scheme on home and siteurl, and check every site.
	var wrong []string
	for _, s := range f.sites {
		t, ok := byID[s.BlogID]
		if !ok {
			continue
		}
		scheme := strings.SplitN(t.URL, "://", 2)[0]
		if _, err := f.sql(fmt.Sprintf(`UPDATE %soptions SET option_value = CONCAT(%s, SUBSTRING(option_value, LOCATE('://', option_value)))
			WHERE option_name IN ('home', 'siteurl') AND option_value LIKE %s`,
			s.Prefix, sqlString(scheme), sqlString("%://"+strings.TrimPrefix(strings.TrimPrefix(t.URL, "https://"), "http://")+"%"))); err != nil {
			return err
		}
		home, err := f.sql(fmt.Sprintf("SELECT option_value FROM %soptions WHERE option_name = 'home'", s.Prefix))
		if err != nil {
			return err
		}
		if strings.TrimSuffix(home, "/") != t.URL {
			wrong = append(wrong, fmt.Sprintf("site %d home is %s, want %s", s.BlogID, home, t.URL))
		}
		f.report.Local = append(f.report.Local, LocalSite{BlogID: s.BlogID, From: homes[s.BlogID], To: t.URL})
	}
	f.report.add(Check{Name: "site addresses point at the local environment", Status: statusIf(len(wrong) > 0, Fail),
		Count: int64(len(wrong)), Detail: strings.Join(wrong, "; ")})

	// Later WP-CLI calls address sites by their new URLs.
	for i := range f.sites {
		if t, ok := byID[f.sites[i].BlogID]; ok {
			f.sites[i].url = t.Host + t.Path
		}
	}
	f.step("rewrote %d URLs for %d local addresses, the main site at %s", total, len(targets), main.URL)
	return nil
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func stripScheme(s string) string {
	for _, p := range []string{"https://", "http://", "//"} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
