// Package localurl works out the local address of each site, so the
// sandbox can rewrite production URLs for a local environment.
//
// The main site gets the local URL. A subsite that has its own path keeps
// that path under the local URL. A subsite on its own domain gets a folder
// named after the domain. For example, with the local URL
// https://news.local:
//
//	blog 1  www.example-newspaper.com/          -> https://news.local
//	blog 2  www.example-sports.com/             -> https://news.local/example-sports
//	blog 3  www.example-newspaper.com/elections/ -> https://news.local/elections
package localurl

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Site is one site's production address.
type Site struct {
	BlogID int
	Domain string // such as www.example-newspaper.com
	Path   string // such as / or /elections/
}

// Target is one site's local address.
type Target struct {
	BlogID int
	URL    string // with scheme, no trailing slash
	Host   string
	Path   string // with leading and trailing slash
}

// Parse checks a local URL and returns it without a trailing slash.
func Parse(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not a URL such as https://news.local", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q must not have a query or fragment", raw)
	}
	return strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// Targets returns the local address of every site. overrides maps blog IDs
// to local URLs that replace the derived ones.
func Targets(localURL string, sites []Site, overrides map[int]string) ([]Target, error) {
	base, err := Parse(localURL)
	if err != nil {
		return nil, err
	}
	var out []Target
	used := map[string]int{}
	for _, s := range sites {
		raw, ok := overrides[s.BlogID]
		switch {
		case ok:
		case s.BlogID == 1:
			raw = base
		case strings.Trim(s.Path, "/") != "":
			raw = base + "/" + strings.Trim(s.Path, "/")
		default:
			raw = base + "/" + Slug(s.Domain)
		}
		u, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("site %d: %w", s.BlogID, err)
		}
		if other, dup := used[u]; dup {
			if ok {
				return nil, fmt.Errorf("sites %d and %d both map to %s", other, s.BlogID, u)
			}
			u = fmt.Sprintf("%s-%d", u, s.BlogID)
		}
		used[u] = s.BlogID
		p, _ := url.Parse(u)
		out = append(out, Target{BlogID: s.BlogID, URL: u, Host: p.Host, Path: strings.TrimSuffix(p.Path, "/") + "/"})
	}
	return out, nil
}

// skipLabels are host name labels that say which environment a host is,
// not which site.
var skipLabels = map[string]bool{
	"www": true, "stage": true, "staging": true, "dev": true, "develop": true, "development": true,
	"test": true, "qa": true, "uat": true, "preprod": true, "prod": true, "vip": true, "local": true,
}

var nonSlug = regexp.MustCompile(`[^a-z0-9-]+`)

// Slug names a folder after a domain: www.example-sports.com becomes
// example-sports.
func Slug(domain string) string {
	host := strings.ToLower(strings.Split(domain, ":")[0])
	labels := strings.Split(host, ".")
	for i, l := range labels {
		if i < len(labels)-1 && !skipLabels[l] {
			if s := strings.Trim(nonSlug.ReplaceAllString(l, "-"), "-"); s != "" {
				return s
			}
		}
	}
	return "site"
}

// Suggest proposes a local URL from the main site's domain:
// www.example-newspaper.com becomes https://example-newspaper.local.
func Suggest(mainDomain string) string {
	return "https://" + Slug(mainDomain) + ".local"
}

// Replacement is one search-replace pair, without the scheme: the text
// after // in a URL.
type Replacement struct {
	From, To string
}

// Replacements turns production addresses into search-replace pairs,
// longest first, so www.example.com/elections is replaced before
// www.example.com. sources maps each blog ID to its production addresses,
// such as the wp_blogs entry and the home option, with or without scheme.
func Replacements(targets []Target, sources map[int][]string) ([]Replacement, error) {
	seen := map[string]string{}
	for _, t := range targets {
		to := strings.TrimPrefix(strings.TrimPrefix(t.URL, "https://"), "http://")
		for _, src := range sources[t.BlogID] {
			from := strings.TrimSuffix(stripScheme(src), "/")
			if from == "" || from == to {
				continue
			}
			if prev, ok := seen[from]; ok && prev != to {
				return nil, fmt.Errorf("the address %s belongs to two sites; set their local URLs under local.sites", from)
			}
			seen[from] = to
		}
	}
	if len(seen) == 0 {
		return nil, errors.New("no production addresses to replace")
	}
	out := make([]Replacement, 0, len(seen))
	for from, to := range seen {
		out = append(out, Replacement{From: from, To: to})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].From) != len(out[j].From) {
			return len(out[i].From) > len(out[j].From)
		}
		return out[i].From < out[j].From
	})
	return out, nil
}

func stripScheme(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://", "http://", "//"} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}
