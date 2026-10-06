package sqldump

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psorensen/WP-Bonsai/internal/synth"
)

// TestMariaDBRoundTrip parses each dump, writes it back with rows regrouped
// into new INSERT statements, imports the original and the copy into MariaDB,
// and compares every table's row count and checksum.
//
// It needs Docker and runs only when BONSAI_MARIADB=1.
func TestMariaDBRoundTrip(t *testing.T) {
	if os.Getenv("BONSAI_MARIADB") != "1" {
		t.Skip("set BONSAI_MARIADB=1 to run the MariaDB round-trip test (needs Docker)")
	}
	db := startMariaDB(t)

	fixture, err := os.ReadFile("../../testdata/edge-cases.sql")
	if err != nil {
		t.Fatal(err)
	}
	synthDefault, _ := synthDump(t, synth.Options{Seed: 11, Posts: 400})
	synthOdd, _ := synthDump(t, synth.Options{Seed: 12, Posts: 400, MaxInsertBytes: 8192,
		CompleteInsert: true, HexBlob: true, Triggers: true})

	cases := []struct {
		name  string
		input []byte
		max   int
	}{
		{"fixture-one-row-per-insert", fixture, 1},
		{"fixture-1mb", fixture, DefaultMaxInsertBytes},
		{"synth-2kb", synthDefault, 2048},
		{"synth-64kb", synthDefault, 64 << 10},
		{"synth-complete-insert-hexblob", synthOdd, 4096},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			w := NewWriter(&out, c.max)
			p := NewParser(bytes.NewReader(c.input))
			for {
				it, err := p.Next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					t.Fatal(err)
				}
				if err := w.Write(it); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(out.Bytes(), c.input) {
				t.Fatal("regrouped output is identical to the input; the test would prove nothing")
			}

			orig := fmt.Sprintf("orig_%d", i)
			copy := fmt.Sprintf("copy_%d", i)
			db.importDump(t, orig, c.input)
			db.importDump(t, copy, out.Bytes())

			origTables := db.query(t, "SELECT table_name, table_type FROM information_schema.tables WHERE table_schema='"+orig+"' ORDER BY 1")
			copyTables := db.query(t, "SELECT table_name, table_type FROM information_schema.tables WHERE table_schema='"+copy+"' ORDER BY 1")
			if origTables != copyTables {
				t.Fatalf("tables differ:\n%s\nvs\n%s", origTables, copyTables)
			}
			for _, kind := range []string{"routines", "triggers"} {
				col := map[string]string{"routines": "routine_schema", "triggers": "trigger_schema"}[kind]
				a := db.query(t, fmt.Sprintf("SELECT COUNT(*) FROM information_schema.%s WHERE %s='%s'", kind, col, orig))
				b := db.query(t, fmt.Sprintf("SELECT COUNT(*) FROM information_schema.%s WHERE %s='%s'", kind, col, copy))
				if a != b {
					t.Errorf("%s: %s in original, %s in copy", kind, a, b)
				}
			}

			total := 0
			for _, line := range strings.Split(origTables, "\n") {
				name, typ, _ := strings.Cut(line, "\t")
				if typ != "BASE TABLE" {
					continue
				}
				q := func(schema string) string {
					tbl := "`" + schema + "`.`" + strings.ReplaceAll(name, "`", "``") + "`"
					count := db.query(t, "SELECT COUNT(*) FROM "+tbl)
					sum := db.query(t, "CHECKSUM TABLE "+tbl+" EXTENDED")
					_, sum, _ = strings.Cut(sum, "\t")
					return count + " rows, checksum " + sum
				}
				a, b := q(orig), q(copy)
				if a != b {
					t.Errorf("table %s: original %s, copy %s", name, a, b)
				}
				var n int
				fmt.Sscan(a, &n)
				total += n
			}
			if total == 0 {
				t.Fatal("no rows imported; the test would prove nothing")
			}
			t.Logf("%d tables, %d rows match", strings.Count(origTables, "\n")+1, total)
		})
	}
}

type mariaDB struct{ id string }

func startMariaDB(t *testing.T) *mariaDB {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1", "mariadb:11.4").Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	db := &mariaDB{id: strings.TrimSpace(string(out))}
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", db.id).Run() })

	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := exec.Command("docker", "exec", db.id, "healthcheck.sh", "--connect", "--innodb_initialized").Run()
		if err == nil {
			return db
		}
		if time.Now().After(deadline) {
			t.Fatalf("MariaDB did not start: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func (db *mariaDB) importDump(t *testing.T, schema string, dump []byte) {
	t.Helper()
	db.query(t, "CREATE DATABASE `"+schema+"`")
	path := filepath.Join(t.TempDir(), schema+".sql")
	if err := os.WriteFile(path, dump, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("docker", "exec", "-i", db.id, "mariadb", "-uroot", schema)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("import into %s: %v\n%s", schema, err, out)
	}
}

func (db *mariaDB) query(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", db.id, "mariadb", "-uroot", "-N", "-B", "-e", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("query %q: %v\n%s", sql, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}
