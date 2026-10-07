package sqldump

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/psorensen/WP-Bonsai/internal/synth"
)

// parseAll reads every item and returns copies of them.
func parseAll(t testing.TB, input []byte) []Item {
	t.Helper()
	items, err := tryParseAll(input)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func tryParseAll(input []byte) ([]Item, error) {
	p := NewParser(bytes.NewReader(input))
	var items []Item
	for {
		it, err := p.Next()
		if errors.Is(err, io.EOF) {
			return items, nil
		}
		if err != nil {
			return items, err
		}
		items = append(items, copyItem(it))
	}
}

func copyItem(it *Item) Item {
	c := *it
	c.Raw = bytes.Clone(it.Raw)
	c.Lead = c.Raw[:len(it.Lead)]
	c.Body = bytes.Clone(it.Body)
	c.Columns = append([]string(nil), it.Columns...)
	c.Fields = nil
	for _, f := range it.Fields {
		c.Fields = append(c.Fields, Field{Type: f.Type, Raw: bytes.Clone(f.Raw)})
	}
	c.delim = bytes.Clone(it.delim)
	return c
}

func joinRaw(items []Item) []byte {
	var b bytes.Buffer
	for _, it := range items {
		b.Write(it.Raw)
	}
	return b.Bytes()
}

func synthDump(t testing.TB, o synth.Options) ([]byte, synth.Stats) {
	t.Helper()
	var b bytes.Buffer
	stats, err := synth.Write(&b, o)
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), stats
}

var synthVariants = map[string]synth.Options{
	"default":         {Seed: 1, Posts: 200},
	"small-inserts":   {Seed: 2, Posts: 200, MaxInsertBytes: 4096},
	"complete-insert": {Seed: 3, Posts: 200, CompleteInsert: true, HexBlob: true, Triggers: true},
}

func TestVerbatimRoundTripSynthetic(t *testing.T) {
	for name, o := range synthVariants {
		t.Run(name, func(t *testing.T) {
			input, stats := synthDump(t, o)
			items := parseAll(t, input)
			if got := joinRaw(items); !bytes.Equal(got, input) {
				t.Fatalf("round trip differs: got %d bytes, want %d", len(got), len(input))
			}
			rows := map[string]int{}
			creates := map[string]bool{}
			for _, it := range items {
				switch it.Kind {
				case Row:
					rows[it.Table]++
				case Statement:
					if it.Type == StmtCreateTable {
						creates[it.Table] = true
					}
					if it.Type == StmtInsert {
						t.Errorf("INSERT not split into rows at offset %d", it.Offset)
					}
				}
			}
			for table, want := range stats {
				if rows[table] != want {
					t.Errorf("%s: parsed %d rows, want %d", table, rows[table], want)
				}
				if !creates[table] {
					t.Errorf("%s: no CREATE TABLE found", table)
				}
			}
		})
	}
}

func TestVerbatimRoundTripFixture(t *testing.T) {
	input, err := os.ReadFile("../../testdata/edge-cases.sql")
	if err != nil {
		t.Fatal(err)
	}
	items := parseAll(t, input)
	if !bytes.Equal(joinRaw(items), input) {
		t.Fatal("round trip differs")
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, 0)
	for i := range items {
		if err := w.Write(&items[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), input) {
		t.Fatal("verbatim writer output differs from input")
	}
}

func TestParseItems(t *testing.T) {
	input := "-- comment\n/*!40101 SET NAMES utf8mb4 */;\n" +
		"CREATE TABLE IF NOT EXISTS `db`.`wp_x` (`a` int, `b` text) ENGINE=InnoDB;\n" +
		"DROP TABLE IF EXISTS `wp_y`;\n" +
		"INSERT IGNORE INTO `wp_x` (`a`, `b`) VALUES (1,'a;b'),\n( -2 , NULL ),(3.5e2,_binary 'x\\'y'),(0xFF,X'00'),(b'101',CURRENT_TIMESTAMP),(NOW(),CONCAT('a',')'));\n" +
		"INSERT INTO t SELECT * FROM u;\n" +
		"REPLACE INTO \"q\"\"t\" VALUES (1) ON DUPLICATE KEY UPDATE a=1;\n" +
		"DELIMITER ;;\nCREATE TRIGGER x BEFORE INSERT ON t FOR EACH ROW BEGIN SET @a = 1; END;;\nINSERT INTO t VALUES (1),(2);;\nDELIMITER ;\n" +
		"-- end\n"

	items := parseAll(t, []byte(input))
	if !bytes.Equal(joinRaw(items), []byte(input)) {
		t.Fatal("round trip differs")
	}

	type want struct {
		kind  Kind
		typ   StmtType
		table string
		body  string
	}
	wants := []want{
		{Statement, StmtOther, "", "/*!40101 SET NAMES utf8mb4 */;"},
		{Statement, StmtCreateTable, "wp_x", ""},
		{Statement, StmtDropTable, "wp_y", "DROP TABLE IF EXISTS `wp_y`;"},
		{InsertHeader, 0, "wp_x", "INSERT IGNORE INTO `wp_x` (`a`, `b`) VALUES"},
		{Row, 0, "wp_x", "(1,'a;b')"},
		{Row, 0, "wp_x", "( -2 , NULL )"},
		{Row, 0, "wp_x", "(3.5e2,_binary 'x\\'y')"},
		{Row, 0, "wp_x", "(0xFF,X'00')"},
		{Row, 0, "wp_x", "(b'101',CURRENT_TIMESTAMP)"},
		{Row, 0, "wp_x", "(NOW(),CONCAT('a',')'))"},
		{InsertEnd, 0, "wp_x", ""},
		{Statement, StmtInsert, "", "INSERT INTO t SELECT * FROM u;"},
		{InsertHeader, 0, `q"t`, ""},
		{Row, 0, `q"t`, "(1)"},
		{InsertEnd, 0, `q"t`, "ON DUPLICATE KEY UPDATE a=1"},
		{Statement, StmtDelimiter, "", "DELIMITER ;;"},
		{Statement, StmtCreateTrigger, "t", "CREATE TRIGGER x BEFORE INSERT ON t FOR EACH ROW BEGIN SET @a = 1; END;;"},
		{InsertHeader, 0, "t", ""},
		{Row, 0, "t", "(1)"},
		{Row, 0, "t", "(2)"},
		{InsertEnd, 0, "t", ""},
		{Statement, StmtDelimiter, "", "DELIMITER ;"},
		{Trailer, 0, "", ""},
	}
	if len(items) != len(wants) {
		for _, it := range items {
			t.Logf("%s %q", it.Kind, it.Body)
		}
		t.Fatalf("got %d items, want %d", len(items), len(wants))
	}
	for i, w := range wants {
		it := items[i]
		if it.Kind != w.kind || it.Type != w.typ || it.Table != w.table {
			t.Errorf("item %d: got %s/%d/%q, want %s/%d/%q", i, it.Kind, it.Type, it.Table, w.kind, w.typ, w.table)
		}
		if w.body != "" && string(it.Body) != w.body {
			t.Errorf("item %d: body %q, want %q", i, it.Body, w.body)
		}
	}

	if got := strings.Join(items[3].Columns, ","); got != "a,b" {
		t.Errorf("columns = %q", got)
	}
	types := []FieldType{}
	for _, it := range items[4:10] {
		for _, f := range it.Fields {
			types = append(types, f.Type)
		}
	}
	wantTypes := []FieldType{FieldNumber, FieldString, FieldNumber, FieldNull, FieldNumber, FieldString,
		FieldHex, FieldHex, FieldBit, FieldExpr, FieldExpr, FieldExpr}
	if len(types) != len(wantTypes) {
		t.Fatalf("field types %v, want %v", types, wantTypes)
	}
	for i := range types {
		if types[i] != wantTypes[i] {
			t.Errorf("field %d: type %d, want %d", i, types[i], wantTypes[i])
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unterminated string":    "INSERT INTO t VALUES (1,'abc",
		"truncated insert":       "INSERT INTO t VALUES (1),(2)",
		"truncated statement":    "CREATE TABLE t (a int)",
		"unterminated comment":   "/* abc",
		"empty value":            "INSERT INTO t VALUES (1,,2);",
		"garbage between tuples": "INSERT INTO t VALUES (1) (2);",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tryParseAll([]byte(input))
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("got %v, want a ParseError", err)
			}
		})
	}
}

func TestFieldValues(t *testing.T) {
	cases := []struct {
		raw  string
		typ  FieldType
		want string
	}{
		{`'plain'`, FieldString, "plain"},
		{`'it\'s'`, FieldString, "it's"},
		{`'it''s'`, FieldString, "it's"},
		{`"dq \"x\""`, FieldString, `dq "x"`},
		{`'a\0b\nc\rd\te\Zf\\g'`, FieldString, "a\x00b\nc\rd\te\x1af\\g"},
		{`'50\% \_x \q'`, FieldString, `50\% \_x q`},
		{`_binary 'x'`, FieldString, "x"},
		{`0x414243`, FieldHex, "ABC"},
		{`X'4142'`, FieldHex, "AB"},
		{`0xA`, FieldHex, "\n"},
		{`-12.5`, FieldNumber, "-12.5"},
	}
	for _, c := range cases {
		got, err := Field{Type: c.typ, Raw: []byte(c.raw)}.AppendValue(nil)
		if err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.raw, got, c.want)
		}
	}

	for raw, want := range map[string]uint64{"42": 42, "'42'": 42} {
		typ := FieldNumber
		if raw[0] == '\'' {
			typ = FieldString
		}
		if got, err := (Field{Type: typ, Raw: []byte(raw)}).Uint64(); err != nil || got != want {
			t.Errorf("Uint64(%s) = %d, %v", raw, got, err)
		}
	}
	if _, err := (Field{Type: FieldString, Raw: []byte("'12a'")}).Uint64(); err == nil {
		t.Error("Uint64('12a') did not fail")
	}
}

// TestRegroupedRows checks that the regrouping writer keeps every row, in
// order, and respects the size limit.
func TestRegroupedRows(t *testing.T) {
	input, _ := synthDump(t, synth.Options{Seed: 7, Posts: 300, MaxInsertBytes: 64 << 10, Triggers: true})
	for _, max := range []int{1, 2048, 1 << 20, 64 << 20} {
		var out bytes.Buffer
		w := NewWriter(&out, max)
		in := parseAll(t, input)
		for i := range in {
			if err := w.Write(&in[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}

		got := parseAll(t, out.Bytes())
		if a, b := rowBodies(in), rowBodies(got); a != b {
			t.Fatalf("max %d: rows differ after regrouping", max)
		}
		var stmt, rows int
		for _, it := range got {
			switch it.Kind {
			case InsertHeader:
				stmt = len(it.Body) + len(it.delim)
				rows = 0
			case Row:
				stmt += len(it.Raw)
				rows++
			case InsertEnd:
				if stmt > max && rows > 1 {
					t.Errorf("max %d: statement of %d bytes with %d rows", max, stmt, rows)
				}
			}
		}
	}
}

func rowBodies(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		if it.Kind == Row {
			b.WriteString(it.Table)
			b.Write(it.Body)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestDroppedRows checks that an INSERT with every row left out writes nothing.
func TestDroppedRows(t *testing.T) {
	input := "LOCK TABLES `t` WRITE;\nINSERT INTO `t` VALUES (1),(2);\nINSERT INTO `t` VALUES (3);\nUNLOCK TABLES;\n"
	var out bytes.Buffer
	w := NewWriter(&out, DefaultMaxInsertBytes)
	for _, it := range parseAll(t, []byte(input)) {
		if it.Kind == Row && string(it.Body) != "(2)" {
			continue
		}
		if err := w.Write(&it); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "LOCK TABLES `t` WRITE;\nINSERT INTO `t` VALUES (2);\nUNLOCK TABLES;\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func BenchmarkParse(b *testing.B) {
	input, _ := synthDump(b, synth.Options{Seed: 1, Posts: 2000})
	b.SetBytes(int64(len(input)))
	for b.Loop() {
		p := NewParser(bytes.NewReader(input))
		for {
			if _, err := p.Next(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				b.Fatal(err)
			}
		}
	}
}

func TestClassifyTables(t *testing.T) {
	cases := []struct {
		body  string
		typ   StmtType
		table string
	}{
		{"LOCK TABLES `wp_2_posts` WRITE;", StmtLockTables, "wp_2_posts"},
		{"/*!40000 ALTER TABLE `wp_2_posts` DISABLE KEYS */;", StmtAlterTable, "wp_2_posts"},
		{"ALTER TABLE wp_x ADD KEY a (a);", StmtAlterTable, "wp_x"},
		{"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER t1 BEFORE UPDATE ON `wp_2_posts` FOR EACH ROW BEGIN SET NEW.a = 1; END */;;", StmtCreateTrigger, "wp_2_posts"},
		{"CREATE TRIGGER t2 AFTER INSERT ON db.wp_y FOR EACH ROW SET @a = 1;", StmtCreateTrigger, "wp_y"},
		{"/*!40101 SET NAMES utf8mb4 */;", StmtOther, ""},
		{"/*!40000 ALTER TABLE `t` ENABLE KEYS */;", StmtAlterTable, "t"},
		{"CREATE TABLE `t` (`a` int);", StmtCreateTable, "t"},
	}
	for _, c := range cases {
		typ, table := classify([]byte(c.body))
		if typ != c.typ || table != c.table {
			t.Errorf("classify(%q) = %d %q, want %d %q", c.body, typ, table, c.typ, c.table)
		}
	}
}
