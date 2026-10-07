package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/finish"
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
	cfg, cfgName, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	return runBuild(ctx, buildRun{dump: fs.Arg(0), cfg: cfg, cfgName: cfgName, work: *work, out: *out, keepWork: *keepWork})
}

// buildRun is one full build: pass 1 if needed, plan, pass 2, and the
// sandbox finish.
type buildRun struct {
	dump     string
	cfg      *config.Config
	cfgName  string
	work     string
	out      string
	keepWork bool
}

// ensureIndex runs pass 1 unless the work directory already holds an index
// of this dump, and returns the index path.
func ensureIndex(ctx context.Context, dumpPath, work string) (string, error) {
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", err
	}
	indexPath := filepath.Join(work, index.FileName)
	fresh, err := indexMatches(indexPath, dumpPath)
	if err != nil {
		return "", err
	}
	if fresh {
		fmt.Printf("Pass 1: reusing %s\n", indexPath)
		return indexPath, nil
	}
	return indexPath, runPass1(ctx, dumpPath, indexPath)
}

func runBuild(ctx context.Context, r buildRun) error {
	start := time.Now()
	dumpPath, cfg, cfgName, work, out := r.dump, r.cfg, r.cfgName, &r.work, &r.out
	indexPath, err := ensureIndex(ctx, dumpPath, *work)
	if err != nil {
		return err
	}
	if !r.keepWork {
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

	// Pass 2, into a private file in the work directory. It holds
	// unscrubbed data, so it is always deleted, and it never becomes -out.
	d, err := openDump(dumpPath)
	if err != nil {
		return err
	}
	defer d.Close()
	tmp, err := os.CreateTemp(*work, "pass2-*.sql")
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
		return fmt.Errorf("pass 2 wrote different row counts than the plan for %d tables; nothing was written", len(res.Mismatches))
	}
	var written int64
	for _, t := range res.Tables {
		written += t.Written
	}
	fmt.Printf("Pass 2: kept %s (%d rows) in %s; the estimate was %s.\n",
		plan.FormatBytes(res.Bytes), written, time.Since(pass2).Round(time.Second), plan.FormatBytes(p.EstimateBytes))

	// Sandbox finish: scrub, recount, validate, export.
	finishStart := time.Now()
	report, ferr := finish.Run(ctx, tmp.Name(), *out, p, cfg, finish.Options{Log: func(m string) { fmt.Println("Sandbox:", m) }})
	reportPath := strings.TrimSuffix(*out, filepath.Ext(*out)) + ".report.json"
	if report != nil {
		if err := report.WriteJSON(reportPath); err != nil {
			return err
		}
		printReport(report, reportPath)
	}
	if ferr != nil {
		return ferr
	}
	fmt.Printf("\nDone in %s (sandbox %s). Wrote %s (%s).\n", time.Since(start).Round(time.Second),
		time.Since(finishStart).Round(time.Second), *out, plan.FormatBytes(report.OutputBytes))
	fmt.Printf("Log in locally as %q with password %q.\n", report.Admin, finish.AdminPassword)
	fmt.Printf("\nThe raw dump at %s holds production data. Delete it when you no longer need it.\n", dumpPath)
	return nil
}

func printReport(r *finish.Report, path string) {
	fmt.Printf("\nValidation: %s (full report: %s)\n", strings.ToUpper(r.Status), path)
	for _, c := range r.Checks {
		if c.Status == finish.Pass {
			continue
		}
		where := ""
		if c.Site != 0 {
			where = fmt.Sprintf("site %d: ", c.Site)
		}
		fmt.Printf("  %-7s %s%s (%d)", c.Status, where, c.Name, c.Count)
		if c.Detail != "" {
			fmt.Printf(": %s", c.Detail)
		}
		fmt.Println()
	}
	passed := 0
	for _, c := range r.Checks {
		if c.Status == finish.Pass {
			passed++
		}
	}
	fmt.Printf("  %d checks passed.\n", passed)
	if len(r.AfterImport) > 0 {
		fmt.Println("After importing locally, with the project's plugins active, run:")
		for _, c := range r.AfterImport {
			fmt.Println("  " + c)
		}
	}
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
