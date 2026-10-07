package main

import (
	"flag"
	"slices"
	"testing"
)

func TestReorder(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.String("out", "", "")
	fs.Bool("keep-work", false, "")
	got := reorder(fs, []string{"dump.sql", "-keep-work", "-out", "slim.sql", "--out=b.sql"})
	want := []string{"-keep-work", "-out", "slim.sql", "--out=b.sql", "dump.sql"}
	if !slices.Equal(got, want) {
		t.Errorf("reorder = %q, want %q", got, want)
	}
}
