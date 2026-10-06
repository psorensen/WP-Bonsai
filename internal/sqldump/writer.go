package sqldump

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxInsertBytes is the target size of one INSERT statement written by
// a regrouping Writer.
const DefaultMaxInsertBytes = 1 << 20

// Writer writes items back out as a dump.
//
// With MaxInsertBytes of zero it writes each item's Raw unchanged, so a
// dump passed through Parser and Writer comes out byte for byte the same.
//
// With MaxInsertBytes above zero it regroups INSERT rows. It copies the
// statement header and the row tuples as written and starts a new INSERT
// statement before one would grow past MaxInsertBytes. A statement always
// holds at least one row, so a single row larger than the limit gets its own
// statement. An INSERT with no rows written produces no output at all, which
// lets a caller drop rows by not passing them to Write.
type Writer struct {
	w   *bufio.Writer
	max int

	inInsert bool
	header   []byte // INSERT ... VALUES
	delim    []byte
	lead     []byte // whitespace and comments before the first batch
	batch    []byte // comma-separated tuples of the batch being built
	rows     int    // rows in batch
	batches  int    // batches written for the current INSERT
}

// NewWriter returns a Writer. See Writer for the meaning of maxInsertBytes.
func NewWriter(w io.Writer, maxInsertBytes int) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, readBufferSize), max: maxInsertBytes}
}

// Write writes one item. Items must arrive in parser order, though rows may be
// left out.
func (w *Writer) Write(it *Item) error {
	if w.max <= 0 {
		_, err := w.w.Write(it.Raw)
		return err
	}
	switch it.Kind {
	case InsertHeader:
		if w.inInsert {
			return errors.New("sqldump: InsertHeader inside an open INSERT")
		}
		w.inInsert = true
		w.header = append(w.header[:0], it.Body...)
		w.delim = append(w.delim[:0], it.delim...)
		w.lead = append(w.lead[:0], it.Lead...)
		w.batch = w.batch[:0]
		w.rows = 0
		w.batches = 0
		return nil
	case Row:
		if !w.inInsert {
			return errors.New("sqldump: Row outside an INSERT")
		}
		// Size of the statement if this row joins the batch: header, a
		// space, the tuples with commas, and the delimiter.
		size := len(w.header) + 1 + len(w.batch) + 1 + len(it.Body) + len(w.delim)
		if w.rows > 0 && size > w.max {
			if err := w.flush(nil); err != nil {
				return err
			}
		}
		if w.rows > 0 {
			w.batch = append(w.batch, ',')
		}
		w.batch = append(w.batch, it.Body...)
		w.rows++
		return nil
	case InsertEnd:
		if !w.inInsert {
			return errors.New("sqldump: InsertEnd outside an INSERT")
		}
		if len(it.Body) > 0 && w.batches > 0 {
			return fmt.Errorf("sqldump: cannot split an INSERT that ends with %q", it.Body)
		}
		err := w.flush(it.Body)
		w.inInsert = false
		return err
	}
	if w.inInsert {
		return fmt.Errorf("sqldump: %s inside an open INSERT", it.Kind)
	}
	_, err := w.w.Write(it.Raw)
	return err
}

// flush writes the batch as one INSERT statement.
func (w *Writer) flush(clause []byte) error {
	if w.rows == 0 {
		return nil
	}
	if w.batches == 0 {
		w.w.Write(w.lead)
	} else {
		w.w.WriteByte('\n')
	}
	w.w.Write(w.header)
	w.w.WriteByte(' ')
	w.w.Write(w.batch)
	if len(clause) > 0 {
		w.w.WriteByte(' ')
		w.w.Write(clause)
	}
	_, err := w.w.Write(w.delim)
	w.batch = w.batch[:0]
	w.rows = 0
	w.batches++
	return err
}

// Flush writes any buffered data to the underlying writer.
func (w *Writer) Flush() error {
	if w.inInsert {
		return errors.New("sqldump: Flush inside an open INSERT")
	}
	return w.w.Flush()
}
