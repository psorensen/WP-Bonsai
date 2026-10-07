// Package slim runs pass 2: it streams the dump again and writes only the
// rows in the keep set. Schema statements are copied as they are, except for
// tables that are dropped entirely. Kept rows are regrouped into INSERT
// statements of about 1 MB. See SPEC.md, "Pass 2: write".
package slim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/sqldump"
)

// Options controls Write.
type Options struct {
	// MaxInsertBytes is the size of the INSERT statements written. Zero
	// means sqldump.DefaultMaxInsertBytes.
	MaxInsertBytes int
	// Progress, if set, is called about every 64 MB with the number of
	// dump bytes read so far.
	Progress func(read int64)
}

// Result reports what pass 2 wrote.
type Result struct {
	Bytes  int64                   // bytes written
	Tables map[string]*TableResult // by table name
	// Mismatches lists tables whose written row count differs from the
	// plan. Any mismatch means pass 1 and pass 2 disagree about the dump.
	Mismatches []string
}

// TableResult counts the rows of one table.
type TableResult struct {
	Rows    int64 // rows read
	Written int64 // rows written
}

// keyColumns are the columns that identify a row of each core table role.
var keyColumns = map[string][]string{
	"posts":              {"ID"},
	"postmeta":           {"meta_id"},
	"terms":              {"term_id"},
	"term_taxonomy":      {"term_taxonomy_id"},
	"term_relationships": {"object_id", "term_taxonomy_id"},
	"termmeta":           {"meta_id"},
	"comments":           {"comment_ID"},
	"commentmeta":        {"meta_id"},
	"options":            {"option_id"},
	"users":              {"ID"},
	"usermeta":           {"umeta_id"},
}

type filter struct {
	table string
	site  int
	role  string
	rule  plan.Rule
	cols  []string // column names from CREATE TABLE
}

// keepAll reports whether every row is kept without looking at it.
func (f *filter) keepAll() bool {
	return f.rule.Action == config.TableKeep || (f.rule.Action == plan.ActionCore && keyColumns[f.role] == nil)
}

func (f *filter) keepNone() bool {
	return f.rule.Action == config.TableEmpty || f.rule.Action == plan.ActionDrop
}

// Write streams the dump from r and writes the slim dump to w.
func Write(ctx context.Context, r io.Reader, w io.Writer, p *plan.Plan, k *plan.KeepSets, opts Options) (*Result, error) {
	if opts.MaxInsertBytes == 0 {
		opts.MaxInsertBytes = sqldump.DefaultMaxInsertBytes
	}
	filters := map[string]*filter{}
	for _, t := range p.Tables {
		filters[t.Name] = &filter{table: t.Name, site: t.Site, role: t.Role, rule: t.Rule, cols: p.Columns[t.Name]}
	}

	cw := &countingWriter{w: w}
	out := sqldump.NewWriter(cw, opts.MaxInsertBytes)
	res := &Result{Tables: map[string]*TableResult{}}
	parser := sqldump.NewParser(r)

	var (
		cur     *filter
		curRes  *TableResult
		pos     []int // positions of the key columns in the current INSERT
		skipIns bool  // the current INSERT belongs to a dropped or empty table
	)
	var nextProgress int64 = 64 << 20
	for {
		it, err := parser.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if opts.Progress != nil && parser.Offset() >= nextProgress {
			opts.Progress(parser.Offset())
			nextProgress += 64 << 20
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		switch it.Kind {
		case sqldump.Statement:
			switch it.Type {
			case sqldump.StmtCreateTable, sqldump.StmtDropTable, sqldump.StmtLockTables,
				sqldump.StmtAlterTable, sqldump.StmtCreateTrigger:
				if f := filters[it.Table]; f != nil && f.rule.Action == plan.ActionDrop {
					continue
				}
			}
		case sqldump.InsertHeader:
			f := filters[it.Table]
			if f == nil {
				return nil, fmt.Errorf("slim: table %s is not in the index; was the index built from this dump?", it.Table)
			}
			cur, curRes = f, res.table(it.Table)
			skipIns = f.keepNone()
			pos = pos[:0]
			if !skipIns && !f.keepAll() {
				names := it.Columns
				if names == nil {
					names = f.cols
				}
				keys := keyColumns[f.role]
				if f.rule.Action == config.TableFilter {
					keys = []string{f.rule.FilterBy}
				}
				for _, key := range keys {
					i := slices.IndexFunc(names, func(n string) bool { return strings.EqualFold(n, key) })
					if i < 0 {
						return nil, fmt.Errorf("slim: table %s has no column %s", f.table, key)
					}
					pos = append(pos, i)
				}
			}
			if skipIns {
				continue
			}
		case sqldump.Row:
			curRes.Rows++
			if skipIns || !keepRow(cur, k, it, pos) {
				continue
			}
			curRes.Written++
		case sqldump.InsertEnd:
			cur = nil
			if skipIns {
				skipIns = false
				continue
			}
		}
		if err := out.Write(it); err != nil {
			return nil, err
		}
	}
	if opts.Progress != nil {
		opts.Progress(parser.Offset())
	}
	if err := out.Flush(); err != nil {
		return nil, err
	}
	res.Bytes = cw.n

	for _, t := range p.Tables {
		got := int64(0)
		if tr := res.Tables[t.Name]; tr != nil {
			got = tr.Written
		}
		if !t.Approximate && got != t.KeptRows {
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: wrote %d rows, the plan kept %d", t.Name, got, t.KeptRows))
		}
	}
	sort.Strings(res.Mismatches)
	return res, nil
}

// keepRow decides one row.
func keepRow(f *filter, k *plan.KeepSets, it *sqldump.Item, pos []int) bool {
	if f.keepAll() {
		return true
	}
	var ids [2]int64
	for i, p := range pos {
		if p >= len(it.Fields) {
			return false
		}
		v, err := it.Fields[p].Uint64()
		if err != nil {
			return false
		}
		ids[i] = int64(v)
	}
	switch {
	case f.rule.Action == config.TableFilter && f.rule.IDs == "sites":
		return k.HasSite(ids[0])
	case f.rule.Action == config.TableFilter:
		return k.HasPost(f.site, ids[0])
	case f.role == "term_relationships":
		return k.HasTermRelationship(f.site, ids[0], ids[1])
	}
	return k.Has(f.role, f.site, ids[0])
}

func (r *Result) table(name string) *TableResult {
	t := r.Tables[name]
	if t == nil {
		t = &TableResult{}
		r.Tables[name] = t
	}
	return t
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
