package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/plan"
	"github.com/psorensen/WP-Bonsai/internal/slim"
)

func buildCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	cfgPath := fs.String("config", "", "project config (default: bonsai.yml if present, else built-in defaults)")
	out := fs.String("out", "slim.sql", "where to write the slim dump")
	work := fs.String("work", "work", "work directory for the index")
	keepWork := fs.Bool("keep-work", false, "keep the index after the build, for more plan runs")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("build needs one dump file")
	}
	dumpPath := fs.Arg(0)
	start := time.Now()

	cfg, cfgName, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	// Pass 1, unless the work directory already holds an index of this dump.
	if err := os.MkdirAll(*work, 0o700); err != nil {
		return err
	}
	indexPath := filepath.Join(*work, index.FileName)
	fresh, err := indexMatches(indexPath, dumpPath)
	if err != nil {
		return err
	}
	if fresh {
		fmt.Printf("Pass 1: reusing %s\n", indexPath)
	} else {
		if err := runPass1(ctx, dumpPath, indexPath); err != nil {
			return err
		}
	}
	if !*keepWork {
		defer func() {
			os.Remove(indexPath)
			os.Remove(indexPath + ".wal")
			os.Remove(*work) // only if empty
		}()
	}

	p, err := plan.Build(ctx, indexPath, cfg)
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Printf("Plan from %s: %d tables, estimated %s\n", cfgName, len(p.Tables), plan.FormatBytes(p.EstimateBytes))
	keep, err := p.LoadKeepSets(ctx)
	if err != nil {
		return err
	}

	// Pass 2, into a temporary file that replaces -out only on success.
	d, err := openDump(dumpPath)
	if err != nil {
		return err
	}
	defer d.Close()
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".bonsai-*.sql")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	pass2 := time.Now()
	res, err := slim.Write(ctx, d, tmp, p, keep, slim.Options{Progress: progressPrinter("pass 2", d, pass2)})
	fmt.Fprintln(os.Stderr)
	if err == nil {
		err = tmp.Close()
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if len(res.Mismatches) > 0 {
		for _, m := range res.Mismatches {
			fmt.Fprintln(os.Stderr, "  mismatch:", m)
		}
		return fmt.Errorf("pass 2 wrote different row counts than the plan for %d tables; the output was not saved", len(res.Mismatches))
	}
	if err := os.Rename(tmp.Name(), *out); err != nil {
		return err
	}

	var written int64
	for _, t := range res.Tables {
		written += t.Written
	}
	fmt.Printf("Pass 2: wrote %s (%d rows) to %s in %s; the estimate was %s.\n",
		plan.FormatBytes(res.Bytes), written, *out, time.Since(pass2).Round(time.Second), plan.FormatBytes(p.EstimateBytes))
	fmt.Printf("Done in %s.\n\n", time.Since(start).Round(time.Second))
	fmt.Println("WARNING: this file is not scrubbed yet. It still holds personal data from production:")
	fmt.Println("user accounts, emails, and comment authors. Do not share it. The scrub step is milestone 5.")
	fmt.Printf("\nThe raw dump at %s holds production data. Delete it when you no longer need it.\n", dumpPath)
	return nil
}

// indexMatches reports whether indexPath holds an index of the current
// version built from the file at dumpPath.
func indexMatches(indexPath, dumpPath string) (bool, error) {
	if _, err := os.Stat(indexPath); err != nil {
		return false, nil
	}
	st, err := os.Stat(dumpPath)
	if err != nil {
		return false, err
	}
	db, err := index.Open(indexPath)
	if err != nil {
		return false, nil // old schema or not an index: build a new one
	}
	defer db.Close()
	src, err := index.ReadSource(db)
	if err != nil {
		return false, nil
	}
	return src.Matches(index.Source{Size: st.Size(), ModTime: st.ModTime()}), nil
}

func runPass1(ctx context.Context, dumpPath, indexPath string) error {
	d, err := openDump(dumpPath)
	if err != nil {
		return err
	}
	defer d.Close()
	start := time.Now()
	sum, err := index.Build(ctx, d, indexPath, index.Source{Path: d.Path, Size: d.Size, ModTime: d.ModTime},
		index.Options{Progress: progressPrinter("pass 1", d, start)})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		os.Remove(indexPath)
		return err
	}
	fmt.Printf("Pass 1: indexed %s in %s, %d sites, %d tables\n", mb(d.Size), sum.Duration.Round(time.Second), sum.Sites, sum.Tables)
	for _, w := range sum.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	return nil
}

// progressPrinter returns a progress callback that prints to stderr.
func progressPrinter(label string, d *dumpFile, start time.Time) func(int64) {
	return func(int64) {
		read := d.FileRead()
		elapsed := time.Since(start).Seconds()
		rate := float64(read) / (1 << 20) / elapsed
		pct := 100 * float64(read) / float64(d.Size)
		eta := ""
		if rate > 0 && read < d.Size {
			eta = fmt.Sprintf(", about %s left", (time.Duration(float64(d.Size-read)/(1<<20)/rate) * time.Second).Round(time.Second))
		}
		fmt.Fprintf(os.Stderr, "\r%s: %5.1f%% of %s, %.0f MB/s%s   ", label, pct, mb(d.Size), rate, eta)
	}
}
