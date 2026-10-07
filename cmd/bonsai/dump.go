package main

import (
	"bufio"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// dumpFile is an open dump. It reads plain or gzip-compressed SQL and counts
// the file bytes read, so progress works for compressed files too.
type dumpFile struct {
	io.Reader
	Path    string
	Size    int64
	ModTime time.Time

	f    *os.File
	gz   *gzip.Reader
	read atomic.Int64
}

func openDump(path string) (*dumpFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	d := &dumpFile{Path: path, Size: st.Size(), ModTime: st.ModTime(), f: f}
	r := bufio.NewReaderSize(countingReader{f, &d.read}, 1<<20)
	d.Reader = r
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			f.Close()
			return nil, err
		}
		d.gz = gz
		d.Reader = gz
	}
	return d, nil
}

// FileRead returns the number of file bytes read so far.
func (d *dumpFile) FileRead() int64 { return d.read.Load() }

func (d *dumpFile) Close() error {
	if d.gz != nil {
		d.gz.Close()
	}
	return d.f.Close()
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
