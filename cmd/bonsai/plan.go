package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/psorensen/WP-Bonsai/internal/config"
	"github.com/psorensen/WP-Bonsai/internal/index"
	"github.com/psorensen/WP-Bonsai/internal/plan"
)

func planCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath := fs.String("config", "", "project config (default: bonsai.yml if present, else built-in defaults)")
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	all := fs.Bool("all", false, "list every table, including empty ones")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	dir := "work"
	if fs.NArg() > 1 {
		return errors.New("plan takes one work directory")
	}
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	cfg, cfgName, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, index.FileName)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no index at %s; run bonsai index first", path)
	}
	p, err := plan.Build(ctx, path, cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	printPlan(p, cfgName, *all)
	return nil
}

func loadConfig(path string) (*config.Config, string, error) {
	if path == "" {
		if _, err := os.Stat("bonsai.yml"); err == nil {
			path = "bonsai.yml"
		}
	}
	if path == "" {
		return config.Default(), "built-in defaults", nil
	}
	cfg, err := config.Load(path)
	return cfg, path, err
}

func printPlan(p *plan.Plan, cfgName string, all bool) {
	fb := plan.FormatBytes
	multi := len(p.Sites) > 1
	fmt.Printf("Plan from %s, table prefix %q\n\n", cfgName, p.Prefix)

	if multi {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SITE\tADDRESS\tSTATUS\tPOSTS\tKEPT\tSIZE\t")
		for _, s := range p.Sites {
			status := "kept"
			if s.Excluded {
				status = "excluded"
			}
			fmt.Fprintf(w, "%d\t%s%s\t%s\t%d\t%d\t%s\t\n", s.BlogID, s.Domain, s.Path, status, s.Posts, s.KeptPosts, fb(s.Bytes))
		}
		w.Flush()
		fmt.Println()
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	siteCol := func(site int) string {
		if multi {
			return fmt.Sprintf("%d\t", site)
		}
		return ""
	}
	header := "POST TYPE\tRULE\tSOURCE\tTOTAL\tSEEDS\tKEPT\tSIZE WITH META\t"
	if multi {
		header = "SITE\t" + header
	}
	fmt.Fprintln(w, header)
	for _, pt := range p.PostTypes {
		if !all && pt.Kept == 0 && pt.Source == "fixed" {
			continue
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%d\t%d\t%d\t%s\t\n", siteCol(pt.Site), pt.Type, pt.Rule, pt.Source, pt.Total, pt.Seeds, pt.Kept, fb(pt.Bytes))
	}
	w.Flush()

	fmt.Println("\nPosts added by dependencies:")
	if len(p.Added) == 0 {
		fmt.Println("  none")
	}
	for _, a := range p.Added {
		if multi {
			fmt.Printf("  site %-3d %6d  %s\n", a.Site, a.Posts, a.Reason)
		} else {
			fmt.Printf("  %6d  %s\n", a.Posts, a.Reason)
		}
	}

	fmt.Println()
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TABLE\tRULE\tSOURCE\tROWS\tKEPT\tSIZE\t")
	hidden, hiddenRows := 0, int64(0)
	for _, t := range p.Tables {
		if !all && t.KeptRows == 0 && t.Rule.Action != plan.ActionCore {
			hidden++
			hiddenRows += t.Rows
			continue
		}
		rule := t.Rule.Action
		if t.Rule.FilterBy != "" {
			rule += " by " + t.Rule.FilterBy
		}
		kept := fmt.Sprint(t.KeptRows)
		if t.Approximate {
			kept = "~" + kept
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t\n", t.Name, rule, t.Rule.Source, t.Rows, kept, fb(t.Bytes))
	}
	w.Flush()
	if hidden > 0 {
		fmt.Printf("  ... and %d tables with no kept rows (%d rows dropped); use -all to list them\n", hidden, hiddenRows)
	}

	fmt.Printf("\nEstimated size: %s (target %s)\n", fb(p.EstimateBytes), fb(p.TargetBytes))
	if len(p.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, wn := range p.Warnings {
			fmt.Println("  - " + wn)
		}
	}
}
