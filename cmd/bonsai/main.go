// Command bonsai turns a large WordPress SQL dump into a small, scrubbed one.
// See SPEC.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/psorensen/WP-Bonsai/internal/sandbox"
)

const usage = `Usage:
  bonsai           <dump>   interactive: asks a few questions, then builds
  bonsai setup              check Docker and prepare the sandbox image
  bonsai version
  bonsai index     <dump> [-out work/]
  bonsai inspect   [work/]
  bonsai plan      [work/] [-config bonsai.yml] [-json] [-all]
  bonsai build     <dump> [-config bonsai.yml] [-out slim.sql] [-work work/] [-keep-work]
  bonsai roundtrip <dump> [-o out.sql] [-max-insert-bytes N]

Commands:
  index       Pass 1. Read the dump once and write work/index.duckdb.
  inspect     Print the inventory of an index as JSON.
  plan        Build the keep set from the index and a config, and estimate
              the output size. Writes nothing.
  build       Pass 1 if the work directory has no index of this dump, then
              plan, pass 2, and the sandbox finish in Docker: scrub, recount,
              and validate. Writes the scrubbed dump and a .report.json file
              next to it. Deletes the index afterwards unless -keep-work is set.
  roundtrip   Parse a dump and write it back out. With -max-insert-bytes 0 the
              output must match the input byte for byte. Prints statistics.

Dumps can be .sql or .sql.gz files.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	if isDumpFile(os.Args[1]) {
		err = wizardCmd(ctx, os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "bonsai:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "index":
		err = indexCmd(ctx, os.Args[2:])
	case "inspect":
		err = inspectCmd(ctx, os.Args[2:])
	case "build":
		err = buildCmd(ctx, os.Args[2:])
	case "plan":
		err = planCmd(ctx, os.Args[2:])
	case "roundtrip":
		err = roundtrip(os.Args[2:])
	case "setup":
		err = setupSandbox(ctx)
		if err == nil {
			fmt.Println("Ready. Run: bonsai <dump file>")
		}
	case "version", "-v", "--version":
		fmt.Println("bonsai", version)
		return
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

// reorder moves flags in front of positional arguments, so that
// "bonsai index dump.sql -out work" works. Boolean flags take no value.
func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				continue
			}
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, pos...)
}

// isDumpFile reports whether an argument names a dump file rather than a
// command, so "bonsai prod.sql" starts interactive mode.
func isDumpFile(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	if strings.HasSuffix(arg, ".sql") || strings.HasSuffix(arg, ".sql.gz") || strings.HasSuffix(arg, ".gz") {
		return true
	}
	st, err := os.Stat(arg)
	return err == nil && !st.IsDir()
}

// version is set at release time with -ldflags "-X main.version=1.2.3".
var version = "dev"

// setupSandbox checks Docker and builds the sandbox image if needed.
func setupSandbox(ctx context.Context) error {
	if err := sandbox.CheckDocker(ctx); err != nil {
		return fmt.Errorf("%w\nBonsai runs its scrub step in Docker. Start Docker Desktop and try again", err)
	}
	return sandbox.EnsureImage(ctx, func(m string) { fmt.Println("Setup:", m) })
}
