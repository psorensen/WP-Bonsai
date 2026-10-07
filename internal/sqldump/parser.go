// Package sqldump reads and writes MySQL and MariaDB SQL dump files as a stream.
//
// The Parser splits a dump into items. Most statements come back whole. INSERT
// statements come back in parts: a header, one item per row tuple, and an end
// item. Memory use is bounded by the largest single row, never by the size of a
// table or of the file.
//
// Every input byte belongs to exactly one item's Raw, so writing every Raw back
// in order reproduces the input byte for byte.
package sqldump

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Kind says which part of the dump an Item holds.
type Kind uint8

const (
	// Statement is a whole statement that the parser does not split.
	Statement Kind = iota + 1
	// InsertHeader is the start of an INSERT or REPLACE statement, through VALUES.
	InsertHeader
	// Row is one value tuple of an INSERT statement.
	Row
	// InsertEnd closes an INSERT statement and holds its delimiter.
	InsertEnd
	// Trailer is whitespace and comments after the last statement.
	Trailer
)

func (k Kind) String() string {
	switch k {
	case Statement:
		return "Statement"
	case InsertHeader:
		return "InsertHeader"
	case Row:
		return "Row"
	case InsertEnd:
		return "InsertEnd"
	case Trailer:
		return "Trailer"
	}
	return fmt.Sprintf("Kind(%d)", k)
}

// StmtType classifies a Statement item.
type StmtType uint8

const (
	StmtOther StmtType = iota
	StmtCreateTable
	StmtDropTable
	// StmtInsert is an INSERT the parser does not split, such as INSERT ... SELECT.
	StmtInsert
	// StmtDelimiter is a client DELIMITER command.
	StmtDelimiter
	// StmtLockTables is LOCK TABLES. Table is the first table locked.
	StmtLockTables
	// StmtAlterTable is ALTER TABLE, such as mysqldump's DISABLE KEYS lines.
	StmtAlterTable
	// StmtCreateTrigger is CREATE TRIGGER. Table is the table it fires on.
	StmtCreateTrigger
)

// FieldType says how a value in a row tuple is written.
type FieldType uint8

const (
	FieldNull FieldType = iota + 1
	FieldNumber
	FieldString // a quoted string, with an optional introducer such as _binary
	FieldHex    // 0x... or X'...'
	FieldBit    // b'...'
	FieldExpr   // anything else, kept as written
)

// Field is one value of a row tuple. Raw is the value exactly as written.
type Field struct {
	Type FieldType
	Raw  []byte
}

// Item is one part of a dump.
//
// Raw, Lead, Body and the Raw of each Field point into a buffer that the next
// call to Next reuses. Copy anything that must outlive that call.
type Item struct {
	Kind   Kind
	Type   StmtType // for Statement items
	Offset int64    // input offset of Raw[0]

	// Raw is every input byte of the item.
	Raw []byte
	// Lead is the start of Raw before Body: whitespace, comments, and for
	// rows the separating comma.
	Lead []byte
	// Body is the meaningful text. For a Statement it includes the
	// delimiter. For an InsertHeader it runs from INSERT through VALUES. For a
	// Row it is the tuple, parentheses included. For an InsertEnd it is any
	// clause after the last tuple, such as ON DUPLICATE KEY UPDATE, and is
	// usually empty.
	Body []byte

	Table   string   // for CREATE TABLE, DROP TABLE, and INSERT items
	Columns []string // InsertHeader column list, or nil when the INSERT has none
	Fields  []Field  // for Row items

	delim []byte // the delimiter in effect, for InsertHeader and InsertEnd items
}

// ParseError reports malformed input and where it was found.
type ParseError struct {
	Offset int64
	Msg    string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("sqldump: offset %d: %s", e.Offset, e.Msg)
}

const (
	readBufferSize = 1 << 20
	// keepCapacity is the largest item buffer the parser keeps between items.
	// A bigger buffer, left behind by one huge row, is released.
	keepCapacity = 16 << 20
)

// Parser reads items from a dump. Create one with NewParser.
type Parser struct {
	r     *bufio.Reader
	off   int64 // input offset of the next unread byte
	err   error // sticky error
	delim []byte

	cur      []byte // bytes of the item being read
	curStart int64

	inInsert bool
	table    string
	rows     int // rows read so far in the current INSERT

	fieldPos  [][2]int
	fieldType []FieldType
	fields    []Field
	item      Item
}

// NewParser returns a parser that reads from r.
func NewParser(r io.Reader) *Parser {
	return &Parser{r: bufio.NewReaderSize(r, readBufferSize), delim: []byte(";")}
}

// Offset returns the number of input bytes consumed so far.
func (p *Parser) Offset() int64 { return p.off }

// Next returns the next item. It returns io.EOF after the last item.
func (p *Parser) Next() (*Item, error) {
	if p.err != nil {
		return nil, p.err
	}
	if cap(p.cur) > keepCapacity {
		p.cur = nil
	}
	p.cur = p.cur[:0]
	p.curStart = p.off
	p.item = Item{Offset: p.off}

	var err error
	if p.inInsert {
		err = p.nextInValues()
	} else {
		err = p.nextStatement()
	}
	if err == nil {
		err = p.err
	}
	if err != nil {
		p.err = err
		return nil, err
	}
	p.item.Raw = p.cur
	return &p.item, nil
}

func (p *Parser) fail(format string, args ...any) error {
	return &ParseError{Offset: p.off, Msg: fmt.Sprintf(format, args...)}
}

// --- low-level reading ---

// avail returns the buffered unread bytes, reading more when none are
// buffered. An empty result means end of input or a read error in p.err.
func (p *Parser) avail() []byte {
	n := p.r.Buffered()
	if n == 0 {
		if _, err := p.r.Peek(1); err != nil {
			p.setReadErr(err)
			return nil
		}
		n = p.r.Buffered()
	}
	b, _ := p.r.Peek(n)
	return b
}

// peek returns up to n unread bytes without consuming them. It returns fewer
// only at end of input.
func (p *Parser) peek(n int) []byte {
	b, err := p.r.Peek(n)
	if err != nil {
		p.setReadErr(err)
	}
	return b
}

func (p *Parser) setReadErr(err error) {
	if err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) && p.err == nil {
		p.err = err
	}
}

// take moves the next n buffered bytes into the current item.
func (p *Parser) take(n int) {
	b, _ := p.r.Peek(n)
	p.cur = append(p.cur, b...)
	_, _ = p.r.Discard(n)
	p.off += int64(n)
}

func (p *Parser) atEOF() bool { return len(p.avail()) == 0 }

func (p *Parser) nextByte() (byte, bool) {
	b := p.peek(1)
	if len(b) == 0 {
		return 0, false
	}
	return b[0], true
}

// takeLine consumes through the next newline, or to end of input.
func (p *Parser) takeLine() {
	for {
		b := p.avail()
		if len(b) == 0 {
			return
		}
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			p.take(i + 1)
			return
		}
		p.take(len(b))
	}
}

// takeThrough consumes through the next occurrence of sep. It reports false
// when the input ends first.
func (p *Parser) takeThrough(sep []byte) bool {
	keep := len(sep) - 1
	for {
		b := p.avail()
		if len(b) == 0 {
			return false
		}
		if i := bytes.Index(b, sep); i >= 0 {
			p.take(i + len(sep))
			return true
		}
		if len(b) > keep {
			p.take(len(b) - keep)
			continue
		}
		// Fewer than len(sep) bytes are buffered. Read more, or stop at the end.
		if pk := p.peek(len(sep)); len(pk) < len(sep) {
			p.take(len(pk))
			return false
		}
	}
}

// takeQuoted consumes a quoted string, identifier, or literal starting at the
// quote character q. Backslash escapes apply inside ' and " but not inside `.
// A doubled quote character stands for one quote.
func (p *Parser) takeQuoted(q byte) error {
	start := p.off
	p.take(1)
	stops := string([]byte{q, '\\'})
	if q == '`' {
		stops = "`"
	}
	for {
		b := p.avail()
		if len(b) == 0 {
			return &ParseError{Offset: start, Msg: fmt.Sprintf("unterminated %c quote", q)}
		}
		i := bytes.IndexAny(b, stops)
		if i < 0 {
			p.take(len(b))
			continue
		}
		if b[i] == '\\' {
			if i+1 < len(b) {
				p.take(i + 2)
				continue
			}
			p.take(i + 1)
			if p.atEOF() {
				return &ParseError{Offset: start, Msg: fmt.Sprintf("unterminated %c quote", q)}
			}
			p.take(1)
			continue
		}
		p.take(i + 1)
		if c, ok := p.nextByte(); ok && c == q {
			p.take(1) // doubled quote
			continue
		}
		return nil
	}
}

func (p *Parser) atDelimiter() bool {
	return bytes.Equal(p.peek(len(p.delim)), p.delim)
}

// atDashComment reports whether the input starts a "-- " comment. MySQL needs
// whitespace or a control character after the two dashes.
func (p *Parser) atDashComment() bool {
	b := p.peek(3)
	if len(b) < 2 || b[0] != '-' || b[1] != '-' {
		return false
	}
	return len(b) == 2 || b[2] <= ' '
}

// atPlainBlockComment reports whether the input starts a /* */ comment that
// is not an executable comment such as /*!40101 ... */ or /*M!100000 ... */.
// The MariaDB sandbox line /*M!999999\- ... */ counts as a plain comment.
func (p *Parser) atPlainBlockComment() bool {
	b := p.peek(3)
	if len(b) < 2 || b[0] != '/' || b[1] != '*' {
		return false
	}
	if len(b) == 3 && b[2] == '!' {
		return false
	}
	if len(b) == 3 && b[2] == 'M' {
		if bytes.HasPrefix(p.peek(12), []byte(`/*M!999999\-`)) {
			return true
		}
		if pk := p.peek(4); len(pk) == 4 && pk[3] == '!' {
			return false
		}
	}
	return true
}

// skipTrivia consumes whitespace and comments.
func (p *Parser) skipTrivia() error {
	for {
		b := p.avail()
		if len(b) == 0 {
			return nil
		}
		switch c := b[0]; {
		case isSpace(c):
			n := 1
			for n < len(b) && isSpace(b[n]) {
				n++
			}
			p.take(n)
		case c == '#':
			p.takeLine()
		case c == '-' && p.atDashComment():
			p.takeLine()
		case c == '/' && p.atPlainBlockComment():
			start := p.off
			p.take(2)
			if !p.takeThrough([]byte("*/")) {
				return &ParseError{Offset: start, Msg: "unterminated comment"}
			}
		default:
			return nil
		}
	}
}

// scanToDelimiter consumes the rest of a statement through the delimiter.
func (p *Parser) scanToDelimiter() error {
	start := p.curStart
	stops := "'\"`#-/" + string(p.delim[0])
	for {
		b := p.avail()
		if len(b) == 0 {
			if p.err != nil {
				return p.err
			}
			return &ParseError{Offset: start, Msg: "input ends inside a statement; the dump may be truncated"}
		}
		i := bytes.IndexAny(b, stops)
		if i < 0 {
			p.take(len(b))
			continue
		}
		if i > 0 {
			p.take(i)
			continue
		}
		c := b[0]
		switch {
		case c == p.delim[0] && p.atDelimiter():
			p.take(len(p.delim))
			return nil
		case c == '\'' || c == '"' || c == '`':
			if err := p.takeQuoted(c); err != nil {
				return err
			}
		case c == '#' || (c == '-' && p.atDashComment()):
			p.takeLine()
		case c == '/' && p.atPlainBlockComment():
			cs := p.off
			p.take(2)
			if !p.takeThrough([]byte("*/")) {
				return &ParseError{Offset: cs, Msg: "unterminated comment"}
			}
		default:
			p.take(1)
		}
	}
}

// --- statements ---

func (p *Parser) nextStatement() error {
	if err := p.skipTrivia(); err != nil {
		return err
	}
	lead := len(p.cur)
	if p.atEOF() {
		if p.err != nil {
			return p.err
		}
		if lead == 0 {
			return io.EOF
		}
		p.item.Kind = Trailer
		p.item.Lead = p.cur
		return nil
	}

	switch p.peekWord() {
	case "INSERT", "REPLACE":
		ok, err := p.scanInsertHeader(lead)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// Not a VALUES insert. Read it as one statement.
		if err := p.scanToDelimiter(); err != nil {
			return err
		}
		p.setStatement(lead, StmtInsert, "")
		return nil
	case "DELIMITER":
		return p.scanDelimiterCommand(lead)
	}

	if err := p.scanToDelimiter(); err != nil {
		return err
	}
	typ, table := classify(p.cur[lead:])
	p.setStatement(lead, typ, table)
	return nil
}

func (p *Parser) setStatement(lead int, typ StmtType, table string) {
	p.item.Kind = Statement
	p.item.Type = typ
	p.item.Table = table
	p.item.Lead = p.cur[:lead]
	p.item.Body = p.cur[lead:]
}

// scanDelimiterCommand reads a client DELIMITER command. It ends at the end of
// the line, not at a delimiter.
func (p *Parser) scanDelimiterCommand(lead int) error {
	for {
		b := p.avail()
		if len(b) == 0 {
			break
		}
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			p.take(i)
			break
		}
		p.take(len(b))
	}
	line := p.cur[lead:]
	arg := bytes.TrimSpace(line[len("DELIMITER"):])
	if len(arg) == 0 {
		return p.fail("DELIMITER command without a delimiter")
	}
	if i := bytes.IndexAny(arg, " \t"); i >= 0 {
		arg = arg[:i]
	}
	p.delim = append([]byte(nil), arg...)
	p.setStatement(lead, StmtDelimiter, "")
	return nil
}

// peekWord returns the upper-cased bare word at the input, or "" when the
// input does not start with one. It does not consume input.
func (p *Parser) peekWord() string {
	b := p.peek(32)
	n := 0
	for n < len(b) && isWordByte(b[n]) {
		n++
	}
	if n == 0 || n == len(b) && len(b) == 32 {
		return ""
	}
	return strings.ToUpper(string(b[:n]))
}

// takeWord consumes a bare word and returns it upper-cased.
func (p *Parser) takeWord() string {
	start := len(p.cur)
	for {
		b := p.avail()
		n := 0
		for n < len(b) && isWordByte(b[n]) {
			n++
		}
		p.take(n)
		if n < len(b) || len(b) == 0 {
			break
		}
	}
	return strings.ToUpper(string(p.cur[start:]))
}

// takeIdent consumes a bare or quoted identifier and returns its name.
func (p *Parser) takeIdent() (string, bool, error) {
	c, ok := p.nextByte()
	if !ok {
		return "", false, nil
	}
	if c == '`' || c == '"' {
		start := len(p.cur)
		if err := p.takeQuoted(c); err != nil {
			return "", false, err
		}
		return unquoteIdent(p.cur[start:]), true, nil
	}
	if !isWordByte(c) {
		return "", false, nil
	}
	start := len(p.cur)
	p.takeWord()
	return string(p.cur[start:]), true, nil
}

// scanInsertHeader reads "INSERT [modifiers] [INTO] table [(columns)] VALUES".
// It reports false, with the bytes read so far kept in p.cur, when the
// statement has another shape. The caller then reads it as one statement.
func (p *Parser) scanInsertHeader(lead int) (bool, error) {
	p.takeWord()
	for {
		if err := p.skipTrivia(); err != nil {
			return false, err
		}
		switch p.peekWord() {
		case "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY", "IGNORE":
			p.takeWord()
			continue
		case "INTO":
			p.takeWord()
			if err := p.skipTrivia(); err != nil {
				return false, err
			}
		}
		break
	}

	table, ok, err := p.takeIdent()
	if err != nil || !ok {
		return false, err
	}
	if err := p.skipTrivia(); err != nil {
		return false, err
	}
	if c, _ := p.nextByte(); c == '.' {
		p.take(1)
		if err := p.skipTrivia(); err != nil {
			return false, err
		}
		if table, ok, err = p.takeIdent(); err != nil || !ok {
			return false, err
		}
		if err := p.skipTrivia(); err != nil {
			return false, err
		}
	}

	var cols []string
	if c, _ := p.nextByte(); c == '(' {
		p.take(1)
		cols = []string{}
		for {
			if err := p.skipTrivia(); err != nil {
				return false, err
			}
			col, ok, err := p.takeIdent()
			if err != nil || !ok {
				return false, err
			}
			cols = append(cols, col)
			if err := p.skipTrivia(); err != nil {
				return false, err
			}
			c, ok := p.nextByte()
			if !ok {
				return false, nil
			}
			p.take(1)
			if c == ')' {
				break
			}
			if c != ',' {
				return false, nil
			}
		}
		if err := p.skipTrivia(); err != nil {
			return false, err
		}
	}

	if w := p.peekWord(); w != "VALUES" && w != "VALUE" {
		return false, nil
	}
	p.takeWord()
	bodyEnd := len(p.cur)
	if err := p.skipTrivia(); err != nil {
		return false, err
	}
	if c, _ := p.nextByte(); c != '(' {
		return false, nil
	}

	p.item.Kind = InsertHeader
	p.item.Table = table
	p.item.Columns = cols
	p.item.Lead = p.cur[:lead]
	p.item.Body = p.cur[lead:bodyEnd]
	p.item.delim = p.delim
	p.inInsert = true
	p.table = table
	p.rows = 0
	return true, nil
}

// --- rows ---

func (p *Parser) nextInValues() error {
	if err := p.skipTrivia(); err != nil {
		return err
	}
	if p.rows > 0 {
		c, ok := p.nextByte()
		if !ok {
			if p.err != nil {
				return p.err
			}
			return p.fail("input ends inside INSERT INTO `%s`; the dump may be truncated", p.table)
		}
		if c != ',' {
			return p.scanInsertEnd()
		}
		p.take(1)
		if err := p.skipTrivia(); err != nil {
			return err
		}
	}
	if c, _ := p.nextByte(); c != '(' {
		return p.fail("expected ( to start a row of `%s`", p.table)
	}

	tupleStart := len(p.cur)
	p.take(1)
	p.fieldPos = p.fieldPos[:0]
	p.fieldType = p.fieldType[:0]
	for {
		if err := p.skipTrivia(); err != nil {
			return err
		}
		start := len(p.cur)
		typ, err := p.scanValue()
		if err != nil {
			return err
		}
		p.fieldPos = append(p.fieldPos, [2]int{start, len(p.cur)})
		p.fieldType = append(p.fieldType, typ)
		if err := p.skipTrivia(); err != nil {
			return err
		}
		c, ok := p.nextByte()
		if !ok {
			return p.fail("input ends inside a row of `%s`; the dump may be truncated", p.table)
		}
		p.take(1)
		if c == ')' {
			break
		}
		if c != ',' {
			return p.fail("expected , or ) in a row of `%s`, found %q", p.table, c)
		}
	}

	p.fields = p.fields[:0]
	for i, pos := range p.fieldPos {
		p.fields = append(p.fields, Field{Type: p.fieldType[i], Raw: p.cur[pos[0]:pos[1]]})
	}
	p.rows++
	p.item.Kind = Row
	p.item.Table = p.table
	p.item.Lead = p.cur[:tupleStart]
	p.item.Body = p.cur[tupleStart:]
	p.item.Fields = p.fields
	return nil
}

// scanInsertEnd reads what follows the last tuple: the delimiter, or a clause
// such as ON DUPLICATE KEY UPDATE and then the delimiter.
func (p *Parser) scanInsertEnd() error {
	lead := len(p.cur)
	p.item.Kind = InsertEnd
	p.item.Table = p.table
	if p.atDelimiter() {
		p.take(len(p.delim))
		p.item.Body = p.cur[lead:lead]
	} else {
		if w := p.peekWord(); w != "ON" && w != "RETURNING" {
			return p.fail("expected , or the end of INSERT INTO `%s` after a row", p.table)
		}
		if err := p.scanToDelimiter(); err != nil {
			return err
		}
		p.item.Body = bytes.TrimRight(p.cur[lead:len(p.cur)-len(p.delim)], " \t\r\n")
	}
	p.item.Lead = p.cur[:lead]
	p.item.delim = p.cur[len(p.cur)-len(p.delim):]
	p.inInsert = false
	return nil
}

// scanValue consumes one value of a row tuple.
func (p *Parser) scanValue() (FieldType, error) {
	b := p.peek(3)
	if len(b) == 0 {
		return 0, p.fail("input ends inside a row of `%s`", p.table)
	}
	c := b[0]
	switch {
	case c == '\'' || c == '"':
		return FieldString, p.takeQuoted(c)
	case c == '0' && len(b) > 1 && (b[1] == 'x' || b[1] == 'X'):
		p.take(2)
		p.takeWhile(isHexDigit)
		return FieldHex, p.endOfSimpleValue()
	case (c == 'x' || c == 'X' || c == 'b' || c == 'B') && len(b) > 1 && b[1] == '\'':
		p.take(1)
		if err := p.takeQuoted('\''); err != nil {
			return 0, err
		}
		if c == 'b' || c == 'B' {
			return FieldBit, nil
		}
		return FieldHex, nil
	case isDigit(c) || c == '-' || c == '+' || c == '.':
		p.takeWhile(isNumberByte)
		return FieldNumber, p.endOfSimpleValue()
	case c == '_':
		// A charset introducer such as _binary or _utf8mb4 before a string.
		p.takeWord()
		if err := p.skipTrivia(); err != nil {
			return 0, err
		}
		if q, _ := p.nextByte(); q == '\'' || q == '"' {
			return FieldString, p.takeQuoted(q)
		}
		return FieldExpr, p.scanExpr()
	case c == ',' || c == ')':
		return 0, p.fail("empty value in a row of `%s`", p.table)
	case isWordByte(c):
		if p.takeWord() == "NULL" {
			if n, ok := p.nextByte(); !ok || n != '(' {
				return FieldNull, nil
			}
		}
		return FieldExpr, p.scanExpr()
	}
	return FieldExpr, p.scanExpr()
}

// endOfSimpleValue checks that a number or hex value is followed by the end of
// the value. If not, the value is an expression such as 1+1.
func (p *Parser) endOfSimpleValue() error {
	c, ok := p.nextByte()
	if !ok || c == ',' || c == ')' || isSpace(c) {
		return nil
	}
	return p.scanExpr()
}

// scanExpr consumes the rest of a value up to the comma or parenthesis that
// ends it, skipping over quoted text and nested parentheses.
func (p *Parser) scanExpr() error {
	depth := 0
	for {
		b := p.avail()
		if len(b) == 0 {
			return p.fail("input ends inside a row of `%s`", p.table)
		}
		i := bytes.IndexAny(b, "'\"`(),")
		if i < 0 {
			p.take(len(b))
			continue
		}
		if i > 0 {
			p.take(i)
			continue
		}
		switch c := b[0]; c {
		case '\'', '"', '`':
			if err := p.takeQuoted(c); err != nil {
				return err
			}
		case '(':
			depth++
			p.take(1)
		case ')', ',':
			if depth == 0 {
				return nil
			}
			if c == ')' {
				depth--
			}
			p.take(1)
		}
	}
}

func (p *Parser) takeWhile(ok func(byte) bool) {
	for {
		b := p.avail()
		n := 0
		for n < len(b) && ok(b[n]) {
			n++
		}
		p.take(n)
		if n < len(b) || len(b) == 0 {
			return
		}
	}
}

// --- classification ---

// classify finds the type and table name of a whole statement.
func classify(body []byte) (StmtType, string) {
	t := tokens{b: body}
	switch t.word() {
	case "CREATE":
		if t.word() != "TABLE" {
			// CREATE TEMPORARY TABLE, CREATE TRIGGER, CREATE VIEW, and so on.
			break
		}
		if t.peekWord() == "IF" {
			t.word() // IF
			t.word() // NOT
			t.word() // EXISTS
		}
		return StmtCreateTable, t.ident()
	case "DROP":
		w := t.word()
		if w == "TEMPORARY" {
			w = t.word()
		}
		if w != "TABLE" {
			return StmtOther, ""
		}
		if t.peekWord() == "IF" {
			t.word() // IF
			t.word() // EXISTS
		}
		return StmtDropTable, t.ident()
	case "LOCK":
		if w := t.word(); w == "TABLES" || w == "TABLE" {
			return StmtLockTables, t.ident()
		}
	case "ALTER":
		w := t.word()
		if w == "ONLINE" || w == "IGNORE" {
			w = t.word()
		}
		if w == "TABLE" {
			return StmtAlterTable, t.ident()
		}
	}
	if m := triggerRe.FindSubmatch(body); m != nil {
		t := tokens{b: m[1]}
		return StmtCreateTrigger, t.ident()
	}
	return StmtOther, ""
}

// triggerRe finds the table of a CREATE TRIGGER statement, including the
// form mysqldump writes inside executable comments.
var triggerRe = regexp.MustCompile("(?is)^\\s*(?:/\\*M?!\\d*\\s*)?CREATE\\b.*?\\bTRIGGER\\s+\\S+\\s+(?:BEFORE|AFTER)\\s+(?:INSERT|UPDATE|DELETE)\\s+ON\\s+((?:`(?:[^`]|``)+`|\\w+)(?:\\s*\\.\\s*(?:`(?:[^`]|``)+`|\\w+))?)")

// tokens is a small reader for the start of an in-memory statement.
type tokens struct {
	b []byte
	i int
}

func (t *tokens) skip() {
	for t.i < len(t.b) {
		c := t.b[t.i]
		switch {
		case isSpace(c):
			t.i++
		case bytes.HasPrefix(t.b[t.i:], []byte("/*!")) || bytes.HasPrefix(t.b[t.i:], []byte("/*M!")):
			// An executable comment: skip only its opener and version,
			// and read what is inside as SQL.
			t.i = bytes.IndexByte(t.b[t.i:], '!') + t.i + 1
			for t.i < len(t.b) && isDigit(t.b[t.i]) {
				t.i++
			}
		case c == '*' && t.i+1 < len(t.b) && t.b[t.i+1] == '/':
			t.i += 2
		case c == '/' && t.i+1 < len(t.b) && t.b[t.i+1] == '*':
			end := bytes.Index(t.b[t.i+2:], []byte("*/"))
			if end < 0 {
				t.i = len(t.b)
				return
			}
			t.i += end + 4
		default:
			return
		}
	}
}

func (t *tokens) peekWord() string {
	save := t.i
	w := t.word()
	t.i = save
	return w
}

func (t *tokens) word() string {
	t.skip()
	start := t.i
	for t.i < len(t.b) && isWordByte(t.b[t.i]) {
		t.i++
	}
	return strings.ToUpper(string(t.b[start:t.i]))
}

// ident reads a possibly qualified identifier and returns its last part.
func (t *tokens) ident() string {
	var name string
	for {
		t.skip()
		if t.i >= len(t.b) {
			return name
		}
		switch q := t.b[t.i]; {
		case q == '`' || q == '"':
			end := t.i + 1
			for end < len(t.b) {
				if t.b[end] == q {
					if end+1 < len(t.b) && t.b[end+1] == q {
						end += 2
						continue
					}
					break
				}
				end++
			}
			if end >= len(t.b) {
				return name
			}
			name = unquoteIdent(t.b[t.i : end+1])
			t.i = end + 1
		case isWordByte(q):
			start := t.i
			for t.i < len(t.b) && isWordByte(t.b[t.i]) {
				t.i++
			}
			name = string(t.b[start:t.i])
		default:
			return name
		}
		t.skip()
		if t.i < len(t.b) && t.b[t.i] == '.' {
			t.i++
			continue
		}
		return name
	}
}

// --- byte classes ---

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isNumberByte(c byte) bool {
	return isDigit(c) || c == '.' || c == 'e' || c == 'E' || c == '-' || c == '+'
}

// isWordByte reports whether c can appear in an unquoted identifier.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigit(c) || c == '_' || c == '$' || c >= 0x80
}

// unquoteIdent removes the quotes around a ` or " quoted identifier.
func unquoteIdent(b []byte) string {
	if len(b) < 2 {
		return string(b)
	}
	q := b[0]
	inner := b[1 : len(b)-1]
	return strings.ReplaceAll(string(inner), string([]byte{q, q}), string(q))
}
