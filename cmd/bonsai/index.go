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
	if err := fs.Parse(reorder(fs, args)); err != nil {
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
	progress := progressPrinter("pass 1", d, time.Now())
	sum, err := index.Build(ctx, d, path, index.Source{Path: d.Path, Size: d.Size, ModTime: d.ModTime}, index.Options{Progress: progress})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		os.Remove(path)
		return err
	}

	fmt.Printf("Indexed %s in %s.\n", mb(d.Size), sum.Duration.Round(time.Second))
	fmt.Printf("  prefix %q, %d sites, %d tables, %d rows, multisite: %v\n", sum.Prefix, sum.Sites, sum.Tables, sum.Rows, sum.Multisite)
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
