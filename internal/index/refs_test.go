package index

import (
	"slices"
	"testing"
)

func TestMetaRefs(t *testing.T) {
	cases := map[string][]int64{
		"123":                                    {123},
		" 45 ":                                   {45},
		"0":                                      nil,
		"-5":                                     nil,
		"12.5":                                   nil,
		"abc":                                    nil,
		"1,2, 3":                                 {1, 2, 3},
		"1,x,3":                                  nil,
		"[4,5]":                                  {4, 5},
		`["6","7"]`:                              {6, 7},
		`a:2:{i:0;s:3:"123";i:1;s:3:"456";}`:     {123, 456},
		`a:2:{s:3:"one";i:7;s:3:"two";s:1:"x";}`: {7},
		`a:1:{i:0;a:1:{s:2:"id";i:99;}}`:         {99},
		`a:1:{i:0;s:3:"123"`:                     nil,
		`O:8:"stdClass":1:{s:2:"id";i:42;}`:      {42},
		"a:1:{i:0;s:10:\"bad length\";}":         nil,
		"9999999999999999999999":                 nil,
		`a:3:{i:0;b:1;i:1;N;i:2;d:1.5;}`:         nil,
	}
	for in, want := range cases {
		got := metaRefs([]byte(in), nil)
		if !slices.Equal(got, want) {
			t.Errorf("metaRefs(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestContentRefs(t *testing.T) {
	content := `<!-- wp:image {"id":12,"sizeSlug":"large"} -->
<figure><img class="wp-image-12" src="x.jpg"/></figure>
<!-- /wp:image -->
<!-- wp:gallery {"ids":[3,"4"],"linkTo":"none"} /-->
<!-- wp:block {"ref":77} /-->
<!-- wp:navigation {"ref":88,"overlayMenu":"never"} /-->
<!-- wp:media-text {"mediaId":55} --><!-- /wp:media-text -->
<!-- wp:acme/hero {"id":"9","title":"x"} /-->
<!-- wp:paragraph --><p>[gallery ids="5,6" size="large"] and [gallery ids='7']</p><!-- /wp:paragraph -->
<!-- wp:image {"id":12} /-->
<!-- wp:broken {"id": -->`

	got := contentRefs([]byte(content), nil)
	want := []contentRef{
		{12, "block:core/image:id"},
		{3, "block:core/gallery:ids"},
		{4, "block:core/gallery:ids"},
		{77, "block:core/block:ref"},
		{88, "block:core/navigation:ref"},
		{55, "block:core/media-text:mediaId"},
		{9, "block:acme/hero:id"},
		{12, "class:wp-image"},
		{5, "shortcode:gallery"},
		{6, "shortcode:gallery"},
		{7, "shortcode:gallery"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("contentRefs =\n%v\nwant\n%v", got, want)
	}
}
