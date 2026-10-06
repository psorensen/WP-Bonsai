// Command bonsai turns a large WordPress SQL dump into a small, scrubbed one.
// See SPEC.md.
package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/sqldump"
)

const usage = `Usage:
  bonsai roundtrip <dump> [-o out.sql] [-max-insert-bytes N]

Commands:
  roundtrip   Parse a dump and write it back out. With -max-insert-bytes 0 the
              output must match the input byte for byte. Prints statistics.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "roundtrip":
		err = roundtrip(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "bonsai: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bonsai:", err)
		os.Exit(1)
	}
}

func roundtrip(args []string) error {
	fs := flag.NewFlagSet("roundtrip", flag.ExitOnError)
	out := fs.String("o", "", "write the output to this file (default: discard)")
	max := fs.Int("max-insert-bytes", 0, "regroup INSERT rows into statements of about this size; 0 copies the input unchanged")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("roundtrip needs one dump file")
	}

	in, closeIn, err := openDump(fs.Arg(0))
	if err != nil {
		return err
	}
	defer closeIn()

	var dst io.Writer = io.Discard
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		dst = f
	}

	start := time.Now()
	p := sqldump.NewParser(in)
	w := sqldump.NewWriter(dst, *max)
	rows := map[string]int{}
	var statements int
	for {
		it, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch it.Kind {
		case sqldump.Row:
			rows[it.Table]++
		case sqldump.Statement:
			statements++
		}
		if err := w.Write(it); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	elapsed := time.Since(start)
	mb := float64(p.Offset()) / (1 << 20)
	fmt.Printf("read %.1f MB in %s (%.0f MB/s), %d statements\n", mb, elapsed.Round(time.Millisecond), mb/elapsed.Seconds(), statements)
	for table, n := range rows {
		fmt.Printf("  %-40s %d rows\n", table, n)
	}
	return nil
}

// openDump opens a .sql or .sql.gz file.
func openDump(path string) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return gz, func() { gz.Close(); f.Close() }, nil
	}
	return r, func() { f.Close() }, nil
}

// reorder moves flags in front of positional arguments, so that
// "bonsai roundtrip dump.sql -o out.sql" works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}
