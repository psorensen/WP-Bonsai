package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/index"
)

func indexCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	out := fs.String("out", "work", "work directory for the index")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("index needs one dump file")
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}

	d, err := openDump(fs.Arg(0))
	if err != nil {
		return err
	}
	defer d.Close()

	path := filepath.Join(*out, index.FileName)
	start := time.Now()
	progress := func(int64) {
		read := d.FileRead()
		elapsed := time.Since(start).Seconds()
		rate := float64(read) / (1 << 20) / elapsed
		pct := 100 * float64(read) / float64(d.Size)
		eta := ""
		if rate > 0 && read < d.Size {
			eta = fmt.Sprintf(", about %s left", (time.Duration(float64(d.Size-read)/(1<<20)/rate) * time.Second).Round(time.Second))
		}
		fmt.Fprintf(os.Stderr, "\rpass 1: %5.1f%% of %s, %.0f MB/s%s   ", pct, mb(d.Size), rate, eta)
	}
	sum, err := index.Build(ctx, d, path, index.Source{Path: d.Path, Size: d.Size, ModTime: d.ModTime}, index.Options{Progress: progress})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		os.Remove(path)
		return err
	}

	fmt.Printf("Indexed %s in %s.\n", mb(d.Size), sum.Duration.Round(time.Second))
	fmt.Printf("  prefix %q, %d tables, %d rows, multisite: %v\n", sum.Prefix, sum.Tables, sum.Rows, sum.Multisite)
	fmt.Printf("  index: %s\n", path)
	for _, w := range sum.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	fmt.Printf("Next: bonsai inspect %s\n", *out)
	return nil
}

func inspectCmd(ctx context.Context, args []string) error {
	dir := "work"
	if len(args) > 1 {
		return errors.New("inspect takes one work directory")
	}
	if len(args) == 1 {
		dir = args[0]
	}
	path := filepath.Join(dir, index.FileName)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no index at %s; run bonsai index first", path)
	}
	db, err := index.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	inv, err := index.ReadInventory(ctx, db)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(inv)
}

func mb(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
