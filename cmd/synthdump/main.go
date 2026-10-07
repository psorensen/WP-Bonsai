// Command synthdump writes a synthetic WordPress dump for tests and
// benchmarks. It never reads real data.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"

	"github.com/psorensen/WP-Bonsai/internal/synth"
)

func main() {
	var o synth.Options
	out := flag.String("o", "synthetic.sql", "output file")
	flag.Uint64Var(&o.Seed, "seed", 1, "random seed; the same seed gives the same dump")
	flag.IntVar(&o.Posts, "posts", 1000, "number of posts")
	flag.IntVar(&o.MaxInsertBytes, "max-insert-bytes", 0, "INSERT statement size; 0 means 1 MB")
	flag.BoolVar(&o.CompleteInsert, "complete-insert", false, "add column lists to INSERT statements")
	flag.BoolVar(&o.HexBlob, "hex-blob", false, "write binary-looking values as hex literals")
	flag.BoolVar(&o.Triggers, "triggers", false, "add a trigger")
	flag.BoolVar(&o.Subsite, "subsite", false, "add a multisite subsite")
	flag.Parse()

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthdump:", err)
		os.Exit(1)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	stats, err := synth.Write(w, o)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthdump:", err)
		os.Exit(1)
	}
	var rows int
	for _, n := range stats {
		rows += n
	}
	fmt.Printf("wrote %s: %d tables, %d rows\n", *out, len(stats), rows)
}
