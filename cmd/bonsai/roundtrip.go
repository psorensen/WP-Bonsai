package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/sqldump"
)

func roundtrip(args []string) error {
	fs := flag.NewFlagSet("roundtrip", flag.ExitOnError)
	out := fs.String("o", "", "write the output to this file (default: discard)")
	max := fs.Int("max-insert-bytes", 0, "regroup INSERT rows into statements of about this size; 0 copies the input unchanged")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("roundtrip needs one dump file")
	}

	in, err := openDump(fs.Arg(0))
	if err != nil {
		return err
	}
	defer in.Close()

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
