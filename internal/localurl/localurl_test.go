package localurl

import (
	"slices"
	"testing"
)

func TestTargets(t *testing.T) {
	sites := []Site{
		{1, "vip.example-news.me", "/"},
		{2, "www.centralvalley.com", "/"},
		{4, "stage.example-herald.com", "/"},
		{12, "www.example-herald.com", "/voter-guide/"},
		{13, "www.example-herald.com", "/"}, // would collide with site 4
	}
	got, err := Targets("https://news.local/", sites, map[int]string{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{
		1:  "https://news.local",
		2:  "https://news.local/centralvalley",
		4:  "https://news.local/example-herald",
		12: "https://news.local/voter-guide",
		13: "https://news.local/example-herald-13",
	}
	for _, tg := range got {
		if tg.URL != want[tg.BlogID] {
			t.Errorf("site %d: %s, want %s", tg.BlogID, tg.URL, want[tg.BlogID])
		}
		if tg.Host != "news.local" || tg.Path[0] != '/' || tg.Path[len(tg.Path)-1] != '/' {
			t.Errorf("site %d: host %q path %q", tg.BlogID, tg.Host, tg.Path)
		}
	}

	over, err := Targets("http://news.local", sites[:2], map[int]string{2: "https://cv.local/"})
	if err != nil || over[1].URL != "https://cv.local" || over[1].Path != "/" {
		t.Errorf("override: %+v, %v", over, err)
	}
	if _, err := Targets("news.local", sites, nil); err == nil {
		t.Error("a URL without a scheme was accepted")
	}
	if _, err := Targets("https://news.local", sites[:2], map[int]string{2: "https://news.local"}); err == nil {
		t.Error("two sites on one address were accepted")
	}
}

func TestSlugAndSuggest(t *testing.T) {
	for in, want := range map[string]string{
		"www.centralvalley.com":        "centralvalley",
		"vip.example-news.me":          "example-news",
		"stage.printdesk.com":          "printdesk",
		"example.test":                 "example",
		"newsroom-develop.go-host.net": "newsroom-develop",
		"localhost:8080":               "site",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	if Suggest("www.example-news.com") != "https://example-news.local" {
		t.Error("Suggest")
	}
}

func TestReplacements(t *testing.T) {
	targets, _ := Targets("https://news.local", []Site{
		{1, "www.example-herald.com", "/"},
		{12, "www.example-herald.com", "/voter-guide/"},
	}, nil)
	got, err := Replacements(targets, map[int][]string{
		1:  {"www.example-herald.com/", "https://www.example-herald.com"},
		12: {"www.example-herald.com/voter-guide/", "https://www.example-herald.com/voter-guide"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Replacement{
		{"www.example-herald.com/voter-guide", "news.local/voter-guide"},
		{"www.example-herald.com", "news.local"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Replacements = %v, want %v", got, want)
	}
}
