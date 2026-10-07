// Package synth writes synthetic WordPress SQL dumps for tests.
//
// The dumps follow mysqldump and MariaDB dump formatting and contain values
// chosen to stress a parser: escapes, quotes, NUL bytes, delimiters and
// comment markers inside strings, serialized PHP, and block markup. All
// people, emails, and addresses are fake. Output is deterministic for a seed.
package synth

import (
	"bufio"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// Options controls the generated dump.
type Options struct {
	Seed  uint64
	Posts int // number of posts of all types, before attachments and revisions
	// MaxInsertBytes is where an INSERT statement is split, like mysqldump's
	// net_buffer_length. Zero means 1 MB.
	MaxInsertBytes int
	// CompleteInsert adds a column list to every INSERT, like
	// mysqldump --complete-insert.
	CompleteInsert bool
	// HexBlob writes binary-looking values as 0x... literals, like
	// mysqldump --hex-blob.
	HexBlob bool
	// Triggers adds a trigger wrapped in DELIMITER commands.
	Triggers bool
	// Subsite adds the tables of a second multisite site, prefix wp_2_.
	Subsite bool
}

// Stats counts the rows written per table.
type Stats map[string]int

const prefix = "wp_"

// Write writes a dump to w and returns the row count per table.
func Write(w io.Writer, o Options) (Stats, error) {
	if o.MaxInsertBytes == 0 {
		o.MaxInsertBytes = 1 << 20
	}
	if o.Posts == 0 {
		o.Posts = 100
	}
	g := &gen{
		w:     bufio.NewWriter(w),
		o:     o,
		r:     rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15)),
		stats: Stats{},
	}
	g.build()
	g.writeDump()
	if err := g.w.Flush(); err != nil {
		return nil, err
	}
	return g.stats, nil
}

type gen struct {
	w     *bufio.Writer
	o     Options
	r     *rand.Rand
	stats Stats

	tables []*table
}

type table struct {
	name   string
	schema string // column definitions and keys, inside CREATE TABLE ( ... )
	cols   []string
	rows   [][]value
}

// value is one SQL value. A nil value is NULL.
type value any

type hexBytes []byte // written as 0x... when HexBlob is on, else as a string

func (g *gen) add(t *table, row ...value) {
	if len(row) != len(t.cols) {
		panic(fmt.Sprintf("synth: %s row has %d values, want %d", t.name, len(row), len(t.cols)))
	}
	t.rows = append(t.rows, row)
}

// --- content ---

// tricky holds strings that break naive dump parsers.
var tricky = []string{
	`It's a "quoted" word`,
	`back\slash and \n literal`,
	"semi;colon; and ;; double",
	"paren ) ( and '),(' tuple break",
	"-- not a comment",
	"/* not a comment */ and /*!40101 not code */",
	"# not a comment either",
	"line one\nline two\r\nline three",
	"tab\there",
	"nul\x00byte",
	"ctrl-z\x1abyte",
	"unicode: café, Zürich, 東京, Привет, emoji 🌳🌲",
	"`backtick` in text",
	"DELIMITER ;;",
	"INSERT INTO `wp_posts` VALUES (1);",
	"trailing backslash \\",
	"percent 100% and under_score",
	"",
}

var words = strings.Fields(`bonsai tree pot soil root branch leaf trunk wire prune
	water light season moss stone garden maple pine juniper ficus elm bark bud`)

func (g *gen) sentence(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = words[g.r.IntN(len(words))]
	}
	s := strings.Join(parts, " ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func (g *gen) trickyText() string { return tricky[g.r.IntN(len(tricky))] }

// phpSerialize writes a PHP serialized array of strings, as WordPress and ACF
// store them in meta values.
func phpSerialize(items []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "a:%d:{", len(items))
	for i, s := range items {
		fmt.Fprintf(&b, "i:%d;s:%d:\"%s\";", i, len(s), s)
	}
	b.WriteString("}")
	return b.String()
}

// --- data model ---

func (g *gen) build() {
	options := &table{name: prefix + "options", cols: []string{"option_id", "option_name", "option_value", "autoload"}, schema: schemaOptions}
	users := &table{name: prefix + "users", cols: []string{"ID", "user_login", "user_pass", "user_nicename", "user_email", "user_url", "user_registered", "user_activation_key", "user_status", "display_name"}, schema: schemaUsers}
	usermeta := &table{name: prefix + "usermeta", cols: []string{"umeta_id", "user_id", "meta_key", "meta_value"}, schema: schemaUsermeta}
	posts := &table{name: prefix + "posts", cols: postCols, schema: schemaPosts}
	postmeta := &table{name: prefix + "postmeta", cols: []string{"meta_id", "post_id", "meta_key", "meta_value"}, schema: schemaPostmeta}
	terms := &table{name: prefix + "terms", cols: []string{"term_id", "name", "slug", "term_group"}, schema: schemaTerms}
	termTax := &table{name: prefix + "term_taxonomy", cols: []string{"term_taxonomy_id", "term_id", "taxonomy", "description", "parent", "count"}, schema: schemaTermTaxonomy}
	termRel := &table{name: prefix + "term_relationships", cols: []string{"object_id", "term_taxonomy_id", "term_order"}, schema: schemaTermRelationships}
	termmeta := &table{name: prefix + "termmeta", cols: []string{"meta_id", "term_id", "meta_key", "meta_value"}, schema: schemaTermmeta}
	comments := &table{name: prefix + "comments", cols: commentCols, schema: schemaComments}
	commentmeta := &table{name: prefix + "commentmeta", cols: []string{"meta_id", "comment_id", "meta_key", "meta_value"}, schema: schemaCommentmeta}
	links := &table{name: prefix + "links", cols: linkCols, schema: schemaLinks}
	bylines := &table{name: prefix + "example_bylines", cols: []string{"byline_id", "post_id", "byline"}, schema: schemaBylines}
	logs := &table{name: prefix + "example_log", cols: []string{"log_id", "message", "created"}, schema: schemaLog}
	g.tables = []*table{commentmeta, comments, links, options, postmeta, posts, termRel, termTax, termmeta, terms, usermeta, users, bylines, logs}
	defer func() { slices.SortFunc(g.tables, func(a, b *table) int { return strings.Compare(a.name, b.name) }) }()

	base := time.Date(2015, 1, 1, 8, 0, 0, 0, time.UTC)

	// Users.
	nUsers := 5
	for id := 1; id <= nUsers; id++ {
		login := fmt.Sprintf("author%d", id)
		if id == 1 {
			login = "admin"
		}
		g.add(users, id, login, "$P$Bfakehashfakehashfakehash"+fmt.Sprint(id), login,
			login+"@example.com", "", base.Format(time.DateTime), "", 0, "Author "+fmt.Sprint(id))
		role := `a:1:{s:6:"author";b:1;}`
		if id == 1 {
			role = `a:1:{s:13:"administrator";b:1;}`
		}
		g.add(usermeta, len(usermeta.rows)+1, id, prefix+"capabilities", role)
		g.add(usermeta, len(usermeta.rows)+1, id, "description", g.trickyText())
	}

	// Terms: a category tree and a flat tag list.
	type term struct{ id, tt int }
	var cats, tags []term
	for i := 1; i <= 12; i++ {
		parent := 0
		if i > 4 {
			parent = cats[g.r.IntN(4)].tt
		}
		id := len(terms.rows) + 1
		g.add(terms, id, fmt.Sprintf("Category %d", i), fmt.Sprintf("category-%d", i), 0)
		g.add(termTax, id, id, "category", g.trickyText(), parent, 0)
		cats = append(cats, term{id, id})
	}
	for i := 1; i <= 40; i++ {
		id := len(terms.rows) + 1
		g.add(terms, id, fmt.Sprintf("Tag %d ✿", i), fmt.Sprintf("tag-%d", i), 0)
		g.add(termTax, id, id, "post_tag", "", 0, 0)
		tags = append(tags, term{id, id})
	}
	g.add(termmeta, 1, cats[0].id, "color", "#2f6b3a")

	// Posts.
	postID := 0
	nextPost := func() int { postID++; return postID }
	var attachments []int
	addPostFull := func(typ, status, title, content, excerpt, slug string, parent, author int, date time.Time, mime string) int {
		id := nextPost()
		if slug == "" {
			slug = fmt.Sprintf("%s-%d", strings.ReplaceAll(typ, "_", "-"), id)
		}
		d := date.Format(time.DateTime)
		g.add(posts, id, author, d, d, content, title, excerpt, status, "open", "open", "",
			slug, "", "", d, d, "", parent, fmt.Sprintf("https://example-newspaper.test/?p=%d", id),
			0, typ, mime, 0)
		return id
	}
	addPost := func(typ, status, title, content string, parent, author int, date time.Time, mime string) int {
		return addPostFull(typ, status, title, content, g.trickyText(), "", parent, author, date, mime)
	}

	for i := 0; i < g.o.Posts/5+1; i++ {
		date := base.Add(time.Duration(g.r.IntN(3650*24)) * time.Hour)
		id := addPost("attachment", "inherit", fmt.Sprintf("image-%d", i), "", 0, 1+g.r.IntN(nUsers), date, "image/jpeg")
		attachments = append(attachments, id)
		g.add(postmeta, len(postmeta.rows)+1, id, "_wp_attached_file", fmt.Sprintf("2020/01/image-%d.jpg", i))
		meta := hexBytes(fmt.Sprintf(`a:3:{s:5:"width";i:%d;s:6:"height";i:%d;s:4:"file";s:%d:"%s";}`,
			800+g.r.IntN(800), 600+g.r.IntN(600), len(fmt.Sprintf("2020/01/image-%d.jpg", i)), fmt.Sprintf("2020/01/image-%d.jpg", i)))
		g.add(postmeta, len(postmeta.rows)+1, id, "_wp_attachment_metadata", meta)
	}

	// ACF field definitions, stored the way ACF 5 stores them.
	group := addPostFull("acf-field-group", "publish", "Story fields",
		`a:2:{s:8:"location";a:0:{}s:8:"position";s:6:"normal";}`, "story-fields", "group_5f1a2b3c4d000", 0, 1, base, "")
	addPostFull("acf-field", "publish", "Gallery",
		`a:3:{s:4:"type";s:7:"gallery";s:13:"return_format";s:2:"id";s:8:"required";i:0;}`, "gallery", "field_5f1a2b3c4d5e6", group, 1, base, "")
	addPostFull("acf-field", "publish", "Related story",
		`a:2:{s:4:"type";s:11:"post_object";s:9:"post_type";a:1:{i:0;s:4:"post";}}`, "related_story_id", "field_5f1a2b3c4d5e7", group, 1, base, "")

	types := []string{"post", "post", "post", "post", "page", "product", "obituary"}
	statuses := []string{"publish", "publish", "publish", "publish", "draft", "future", "pitch", "private"}
	var pages, published []int
	for i := 0; i < g.o.Posts; i++ {
		typ := types[g.r.IntN(len(types))]
		status := statuses[g.r.IntN(len(statuses))]
		date := base.Add(time.Duration(g.r.IntN(3650*24)) * time.Hour)
		img := attachments[g.r.IntN(len(attachments))]
		content := fmt.Sprintf("<!-- wp:paragraph -->\n<p>%s %s</p>\n<!-- /wp:paragraph -->\n\n"+
			"<!-- wp:image {\"id\":%d,\"sizeSlug\":\"large\"} -->\n<figure class=\"wp-block-image\"><img src=\"https://example-newspaper.test/image.jpg\" class=\"wp-image-%d\"/></figure>\n<!-- /wp:image -->",
			g.sentence(8+g.r.IntN(30)), g.trickyText(), img, img)
		if g.r.IntN(10) == 0 {
			// A long body, so some rows are far bigger than others.
			content += strings.Repeat("<p>"+g.sentence(20)+"</p>\n", 200+g.r.IntN(400))
		}
		parent := 0
		if typ == "page" && len(pages) > 0 && g.r.IntN(3) == 0 {
			parent = pages[g.r.IntN(len(pages))]
		}
		id := addPost(typ, status, g.sentence(4)+" "+g.trickyText(), content, parent, 1+g.r.IntN(nUsers), date, "")
		if typ == "page" {
			pages = append(pages, id)
		}
		if status == "publish" {
			published = append(published, id)
		}

		g.add(postmeta, len(postmeta.rows)+1, id, "_thumbnail_id", fmt.Sprint(img))
		g.add(postmeta, len(postmeta.rows)+1, id, "_edit_lock", fmt.Sprintf("%d:1", date.Unix()))
		if g.r.IntN(3) == 0 {
			gallery := []string{fmt.Sprint(attachments[g.r.IntN(len(attachments))]), fmt.Sprint(attachments[g.r.IntN(len(attachments))])}
			g.add(postmeta, len(postmeta.rows)+1, id, "gallery", phpSerialize(gallery))
			g.add(postmeta, len(postmeta.rows)+1, id, "_gallery", "field_5f1a2b3c4d5e6")
		}
		if len(published) > 1 && g.r.IntN(4) == 0 {
			g.add(postmeta, len(postmeta.rows)+1, id, "related_story_id", fmt.Sprint(published[g.r.IntN(len(published)-1)]))
			g.add(postmeta, len(postmeta.rows)+1, id, "_related_story_id", "field_5f1a2b3c4d5e7")
		}
		if g.r.IntN(3) == 0 {
			g.add(bylines, len(bylines.rows)+1, id, fmt.Sprintf("By Author %d", 1+g.r.IntN(nUsers)))
		}
		if g.r.IntN(8) == 0 {
			g.add(postmeta, len(postmeta.rows)+1, id, "notes", g.trickyText())
		}
		if g.r.IntN(20) == 0 {
			g.add(postmeta, len(postmeta.rows)+1, id, "empty_meta", nil)
		}

		if typ == "post" {
			g.add(termRel, id, cats[g.r.IntN(len(cats))].tt, 0)
			seen := map[int]bool{}
			for range g.r.IntN(4) {
				tt := tags[g.r.IntN(len(tags))].tt
				if !seen[tt] {
					seen[tt] = true
					g.add(termRel, id, tt, 0)
				}
			}
			for range g.r.IntN(3) {
				cid := len(comments.rows) + 1
				cd := date.Add(time.Duration(1+g.r.IntN(72)) * time.Hour).Format(time.DateTime)
				g.add(comments, cid, id, fmt.Sprintf("Reader %d", cid), fmt.Sprintf("reader%d@example.org", cid),
					"", fmt.Sprintf("192.0.2.%d", 1+g.r.IntN(254)), cd, cd, g.sentence(10)+" "+g.trickyText(),
					0, "1", "Mozilla/5.0 (synthetic)", "comment", 0, 0)
				if g.r.IntN(2) == 0 {
					g.add(commentmeta, len(commentmeta.rows)+1, cid, "rating", fmt.Sprint(1+g.r.IntN(5)))
				}
			}
			if g.r.IntN(3) == 0 {
				addPost("revision", "inherit", "Revision", content, id, 1, date, "")
			}
		}
	}

	// Menu items pointing at pages.
	for i := 0; i < 5 && i < len(pages); i++ {
		id := addPost("nav_menu_item", "publish", "", "", 0, 1, base, "")
		g.add(postmeta, len(postmeta.rows)+1, id, "_menu_item_object_id", fmt.Sprint(pages[i]))
		g.add(postmeta, len(postmeta.rows)+1, id, "_menu_item_object", "page")
	}

	// Options.
	front := 0
	if len(pages) > 0 {
		front = pages[0]
	}
	sticky := []string{}
	for i := 0; i < 3 && i < len(published); i++ {
		sticky = append(sticky, fmt.Sprint(published[i]))
	}
	opts := [][2]any{
		{"siteurl", "https://example-newspaper.test"},
		{"home", "https://example-newspaper.test"},
		{"blogname", "Example Newspaper"},
		{"blogdescription", g.trickyText()},
		{"admin_email", "admin@example.com"},
		{"posts_per_page", "10"},
		{"show_on_front", "page"},
		{"page_on_front", fmt.Sprint(front)},
		{"sticky_posts", phpSerialize(sticky)},
		{"_transient_feed_abc123", strings.Repeat("cached ", 50)},
		{"_site_transient_timeout_theme_roots", "1700000000"},
		{"widget_text", `a:2:{i:2;a:1:{s:4:"text";s:23:"It's <b>bold</b>; isn't";}s:12:"_multiwidget";i:1;}`},
		{"big_option", strings.Repeat("x", 120*1024)},
	}
	for i, kv := range opts {
		g.add(options, i+1, kv[0], kv[1], "yes")
	}

	g.add(links, 1, "https://example.org/", "Example link", "", "", "", "Y", 1, 0, base.Format(time.DateTime), "", "", "")

	for i := 1; i <= 200; i++ {
		g.add(logs, i, fmt.Sprintf("event %d: %s", i, g.trickyText()), base.Add(time.Duration(i)*time.Minute).Format(time.DateTime))
	}

	if g.o.Subsite {
		blogs := &table{name: prefix + "blogs", cols: []string{"blog_id", "domain", "path"}, schema: schemaBlogs}
		sub := prefix + "2_"
		subPosts := &table{name: sub + "posts", cols: postCols, schema: schemaPosts}
		subMeta := &table{name: sub + "postmeta", cols: []string{"meta_id", "post_id", "meta_key", "meta_value"}, schema: schemaPostmeta}
		subOptions := &table{name: sub + "options", cols: []string{"option_id", "option_name", "option_value", "autoload"}, schema: schemaOptions}
		g.tables = append(g.tables, blogs, subPosts, subMeta, subOptions)
		g.add(blogs, 1, "example-newspaper.test", "/")
		g.add(blogs, 2, "example-newspaper.test", "/sports/")
		d := base.Format(time.DateTime)
		for id := 1; id <= 3; id++ {
			g.add(subPosts, id, 1, d, d, "Subsite post", "Subsite post", "", "publish", "open", "open", "",
				fmt.Sprintf("sub-%d", id), "", "", d, d, "", 0, "", 0, "post", "", 0)
			g.add(subMeta, id, id, "_edit_lock", "1:1")
		}
		g.add(usermeta, len(usermeta.rows)+1, 2, sub+"capabilities", `a:1:{s:6:"editor";b:1;}`)
		g.add(subOptions, 1, "siteurl", "https://example-newspaper.test/sports", "yes")
		g.add(subOptions, 2, "posts_per_page", "5", "yes")
	}

	// Fill in comment counts and term counts so the data is consistent.
	commentCount := map[int]int{}
	for _, c := range comments.rows {
		commentCount[c[1].(int)]++
	}
	for _, p := range posts.rows {
		p[len(p)-1] = commentCount[p[0].(int)]
	}
	termCount := map[int]int{}
	for _, tr := range termRel.rows {
		termCount[tr[1].(int)]++
	}
	for _, tt := range termTax.rows {
		tt[5] = termCount[tt[0].(int)]
	}
}

// --- output ---

func (g *gen) writeDump() {
	w := g.w
	w.WriteString("/*M!999999\\- enable the sandbox mode */ \n")
	w.WriteString("-- Synthetic WordPress dump written by bonsai internal/synth\n--\n-- Host: localhost    Database: example_newspaper\n")
	w.WriteString("-- ------------------------------------------------------\n-- Server version\t11.4.0-MariaDB\n\n")
	w.WriteString(dumpHeader)

	for _, t := range g.tables {
		fmt.Fprintf(w, "\n--\n-- Table structure for table `%s`\n--\n\n", t.name)
		fmt.Fprintf(w, "DROP TABLE IF EXISTS `%s`;\n", t.name)
		w.WriteString("/*!40101 SET @saved_cs_client     = @@character_set_client */;\n")
		w.WriteString("/*!40101 SET character_set_client = utf8mb4 */;\n")
		fmt.Fprintf(w, "CREATE TABLE `%s` (\n%s\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci;\n", t.name, t.schema)
		w.WriteString("/*!40101 SET character_set_client = @saved_cs_client */;\n")

		fmt.Fprintf(w, "\n--\n-- Dumping data for table `%s`\n--\n\n", t.name)
		fmt.Fprintf(w, "LOCK TABLES `%s` WRITE;\n", t.name)
		fmt.Fprintf(w, "/*!40000 ALTER TABLE `%s` DISABLE KEYS */;\n", t.name)
		g.writeInserts(t)
		fmt.Fprintf(w, "/*!40000 ALTER TABLE `%s` ENABLE KEYS */;\n", t.name)
		w.WriteString("UNLOCK TABLES;\n")
		g.stats[t.name] = len(t.rows)

		if g.o.Triggers && t.name == prefix+"posts" {
			w.WriteString(triggerSQL)
		}
	}

	w.WriteString(dumpFooter)
	w.WriteString("\n-- Dump completed on 2026-10-06 12:00:00\n")
}

func (g *gen) writeInserts(t *table) {
	header := "INSERT INTO `" + t.name + "`"
	if g.o.CompleteInsert {
		header += " (`" + strings.Join(t.cols, "`, `") + "`)"
	}
	header += " VALUES "

	var stmt strings.Builder
	var tuple strings.Builder
	for _, row := range t.rows {
		tuple.Reset()
		tuple.WriteByte('(')
		for i, v := range row {
			if i > 0 {
				tuple.WriteByte(',')
			}
			g.writeValue(&tuple, v)
		}
		tuple.WriteByte(')')

		if stmt.Len() > 0 && stmt.Len()+1+tuple.Len() > g.o.MaxInsertBytes {
			stmt.WriteString(";\n")
			g.w.WriteString(stmt.String())
			stmt.Reset()
		}
		if stmt.Len() == 0 {
			stmt.WriteString(header)
		} else {
			stmt.WriteByte(',')
		}
		stmt.WriteString(tuple.String())
	}
	if stmt.Len() > 0 {
		stmt.WriteString(";\n")
		g.w.WriteString(stmt.String())
	}
}

func (g *gen) writeValue(b *strings.Builder, v value) {
	switch v := v.(type) {
	case nil:
		b.WriteString("NULL")
	case int:
		fmt.Fprint(b, v)
	case string:
		writeString(b, v)
	case hexBytes:
		if g.o.HexBlob {
			if len(v) == 0 {
				b.WriteString("''")
				return
			}
			fmt.Fprintf(b, "0x%X", []byte(v))
			return
		}
		writeString(b, string(v))
	default:
		panic(fmt.Sprintf("synth: unsupported value %T", v))
	}
}

// writeString writes a quoted string the way mysqldump escapes it.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
}

const dumpHeader = `/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!40101 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*M!100616 SET @OLD_NOTE_VERBOSITY=@@NOTE_VERBOSITY, NOTE_VERBOSITY=0 */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;
`

const dumpFooter = `/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*M!100616 SET NOTE_VERBOSITY=@OLD_NOTE_VERBOSITY */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
`

// triggerSQL is formatted like mysqldump --triggers output. The body holds
// semicolons, so it sits inside DELIMITER ;; commands.
const triggerSQL = `/*!50003 SET @saved_cs_client      = @@character_set_client */ ;
/*!50003 SET @saved_cs_results     = @@character_set_results */ ;
/*!50003 SET @saved_col_connection = @@collation_connection */ ;
/*!50003 SET character_set_client  = utf8mb4 */ ;
/*!50003 SET character_set_results = utf8mb4 */ ;
/*!50003 SET collation_connection  = utf8mb4_unicode_520_ci */ ;
/*!50003 SET @saved_sql_mode       = @@sql_mode */ ;
/*!50003 SET sql_mode              = 'NO_AUTO_VALUE_ON_ZERO' */ ;
DELIMITER ;;
/*!50003 CREATE*/ /*!50017 DEFINER=` + "`root`@`localhost`" + `*/ /*!50003 TRIGGER wp_posts_touch BEFORE UPDATE ON wp_posts FOR EACH ROW BEGIN
  SET NEW.post_modified = NOW();
  SET NEW.post_modified_gmt = UTC_TIMESTAMP();
END */;;
DELIMITER ;
/*!50003 SET sql_mode              = @saved_sql_mode */ ;
/*!50003 SET character_set_client  = @saved_cs_client */ ;
/*!50003 SET character_set_results = @saved_cs_results */ ;
/*!50003 SET collation_connection  = @saved_col_connection */ ;
`
