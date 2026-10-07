// Package mariadbtest runs a throwaway MariaDB server in Docker for tests.
package mariadbtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Enabled reports whether MariaDB tests should run. They need Docker and run
// only when BONSAI_MARIADB=1.
func Enabled() bool { return os.Getenv("BONSAI_MARIADB") == "1" }

// Skip skips the test unless MariaDB tests are enabled.
func Skip(t *testing.T) {
	t.Helper()
	if !Enabled() {
		t.Skip("set BONSAI_MARIADB=1 to run MariaDB tests (needs Docker)")
	}
}

// DB is a running MariaDB container.
type DB struct{ id string }

// Start starts a MariaDB 11.4 container that is removed when the test ends.
func Start(t *testing.T) *DB {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "--rm",
		"-e", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1", "mariadb:11.4").Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	db := &DB{id: strings.TrimSpace(string(out))}
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

// Import creates a database and imports a dump into it.
func (db *DB) Import(t *testing.T, schema string, dump []byte) {
	t.Helper()
	db.Query(t, "CREATE DATABASE `"+schema+"`")
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

// Query runs SQL and returns the output, tab-separated, without headers.
func (db *DB) Query(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", db.id, "mariadb", "-uroot", "-N", "-B", "-e", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("query %q: %v\n%s", sql, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}
