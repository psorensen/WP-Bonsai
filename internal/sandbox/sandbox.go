// Package sandbox runs the sandbox finish step in local Docker: a throwaway
// MariaDB server and a WP-CLI container on a network with no internet
// access. See SPEC.md, "Finish in a sandbox".
//
// The scrubber is 10up WP Scrubber (GPL-2.0). It runs inside the WP-CLI
// container and is called only through WP-CLI. None of its code is part of
// the bonsai binary.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Versions baked into the sandbox image.
const (
	mariadbImage    = "mariadb:11.4"
	cliBaseImage    = "wordpress:cli-php8.3"
	scrubberVersion = "1.0.3" // github.com/10up/wp-scrubber tag
)

// dockerfile builds the WP-CLI image: WordPress core plus WP Scrubber,
// loaded as a must-use plugin so nothing has to be activated in the
// database. Building it is the only step that needs the internet.
var dockerfile = `FROM ` + cliBaseImage + `
USER root
RUN mkdir -p /var/www/html/wp-content/mu-plugins && chown -R www-data:www-data /var/www/html
USER www-data
WORKDIR /var/www/html
RUN wp core download --skip-content && mkdir -p wp-content/mu-plugins wp-content/plugins wp-content/themes
RUN wget -qO- https://github.com/10up/wp-scrubber/archive/refs/tags/` + scrubberVersion + `.tar.gz \
	| tar xz -C wp-content/mu-plugins \
	&& mv wp-content/mu-plugins/wp-scrubber-` + scrubberVersion + ` wp-content/mu-plugins/wp-scrubber \
	&& printf '<?php\nrequire_once WPMU_PLUGIN_DIR . "/wp-scrubber/plugin.php";\n' > wp-content/mu-plugins/wp-scrubber-loader.php
LABEL bonsai.scrubber="wp-scrubber ` + scrubberVersion + `"
`

// Image is the tag of the sandbox image. It changes with the Dockerfile,
// so an old image is never reused by mistake.
func Image() string {
	sum := sha256.Sum256([]byte(dockerfile))
	return "bonsai-sandbox:" + hex.EncodeToString(sum[:6])
}

// Sandbox is a running MariaDB container and WP-CLI container.
type Sandbox struct {
	network string
	db      string
	cli     string
	log     func(string)
}

// Start builds the image if needed and starts both containers.
func Start(ctx context.Context, log func(string)) (*Sandbox, error) {
	if log == nil {
		log = func(string) {}
	}
	if err := docker(ctx, nil, nil, "version", "--format", "{{.Server.Version}}"); err != nil {
		return nil, fmt.Errorf("sandbox: Docker is not running: %w", err)
	}
	if err := ensureImage(ctx, log); err != nil {
		return nil, err
	}

	id := randomID()
	s := &Sandbox{network: "bonsai-" + id, db: "bonsai-db-" + id, cli: "bonsai-cli-" + id, log: log}
	// --internal: the containers can reach each other but not the internet.
	if err := docker(ctx, nil, nil, "network", "create", "--internal", s.network); err != nil {
		return nil, fmt.Errorf("sandbox: create network: %w", err)
	}
	if err := docker(ctx, nil, nil, "run", "-d", "--rm", "--name", s.db, "--network", s.network,
		// An anonymous volume, not tmpfs: a big result does not fit in the
		// memory of Docker's VM. --rm deletes the volume with the container.
		"--network-alias", "db", "-v", "/var/lib/mysql",
		"-e", "MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1", "-e", "MARIADB_DATABASE=wordpress",
		mariadbImage, "--max-allowed-packet=256M"); err != nil {
		s.Close()
		return nil, fmt.Errorf("sandbox: start MariaDB: %w", err)
	}
	if err := docker(ctx, nil, nil, "run", "-d", "--rm", "--name", s.cli, "--network", s.network,
		"--entrypoint", "sleep", Image(), "infinity"); err != nil {
		s.Close()
		return nil, fmt.Errorf("sandbox: start WP-CLI: %w", err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := docker(ctx, nil, nil, "exec", s.db, "healthcheck.sh", "--connect", "--innodb_initialized")
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			s.Close()
			return nil, fmt.Errorf("sandbox: MariaDB did not start: %w", err)
		}
		time.Sleep(time.Second)
	}
	return s, nil
}

// EnsureImage builds the sandbox image unless it already exists. It is the
// only step that needs the internet.
func EnsureImage(ctx context.Context, log func(string)) error {
	if log == nil {
		log = func(string) {}
	}
	return ensureImage(ctx, log)
}

func ensureImage(ctx context.Context, log func(string)) error {
	if docker(ctx, nil, nil, "image", "inspect", Image()) == nil {
		return nil
	}
	log("building the sandbox image " + Image() + " (once; needs the internet)")
	var out bytes.Buffer
	if err := docker(ctx, strings.NewReader(dockerfile), &out, "build", "-t", Image(), "-"); err != nil {
		return fmt.Errorf("sandbox: build image: %w\n%s", err, tail(out.String(), 2000))
	}
	return nil
}

// Close removes the containers and the network.
func (s *Sandbox) Close() {
	ctx := context.Background()
	docker(ctx, nil, nil, "rm", "-f", s.db, s.cli)
	docker(ctx, nil, nil, "network", "rm", s.network)
}

// Import loads a dump file into the wordpress database.
func (s *Sandbox) Import(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var out bytes.Buffer
	if err := docker(ctx, f, &out, "exec", "-i", s.db, "mariadb", "-uroot", "wordpress"); err != nil {
		return fmt.Errorf("sandbox: import failed: %w: %s", err, dbErrors(out.String()))
	}
	return nil
}

// SQL runs statements in the wordpress database and returns the output,
// tab-separated, without column names.
func (s *Sandbox) SQL(ctx context.Context, query string) (string, error) {
	var out bytes.Buffer
	if err := docker(ctx, strings.NewReader(query), &out, "exec", "-i", s.db, "mariadb", "-uroot", "-N", "-B", "wordpress"); err != nil {
		return "", fmt.Errorf("sandbox: SQL failed: %w: %s", err, dbErrors(out.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// WP runs a WP-CLI command and returns its output.
func (s *Sandbox) WP(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	full := append([]string{"exec", s.cli, "wp"}, args...)
	if err := docker(ctx, nil, &out, full...); err != nil {
		return "", fmt.Errorf("sandbox: wp %s failed: %w\n%s", strings.Join(args, " "), err, Redact(tail(lastLines(out.String(), 6), 1500)))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Export writes a dump of the wordpress database.
func (s *Sandbox) Export(ctx context.Context, w io.Writer) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "exec", s.db, "mariadb-dump", "-uroot",
		"--single-transaction", "--routines", "--triggers", "--skip-dump-date", "wordpress")
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sandbox: export failed: %w: %s", err, dbErrors(stderr.String()))
	}
	return nil
}

// docker runs a docker command. Output goes to out when it is set.
func docker(ctx context.Context, stdin io.Reader, out *bytes.Buffer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}
	return cmd.Run()
}

func randomID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// lastLines keeps the last n lines of WP-CLI output, where the error is.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// CheckDocker reports an error when Docker is not running.
func CheckDocker(ctx context.Context) error {
	if err := docker(ctx, nil, nil, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("Docker is not running: %w", err)
	}
	return nil
}

// Errors from the sandbox go to the terminal and into the report, which is
// meant to be safe to share. The database holds production data, and
// MariaDB echoes the failing statement with its values, so errors are
// redacted before they leave this package.

var (
	quotedRe = regexp.MustCompile(`'[^']*'`)
	identRe  = regexp.MustCompile(`^'[A-Za-z0-9_$.\-]{1,64}'$`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
)

// dbErrors keeps only the ERROR lines of MariaDB client output, redacted.
// The client also prints the failing statement, which holds row values.
func dbErrors(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ERROR") {
			keep = append(keep, Redact(tail(strings.TrimSpace(line), 300)))
		}
	}
	if len(keep) == 0 {
		return "no error message from MariaDB"
	}
	return strings.Join(keep, "; ")
}

// Redact removes data from an error message. A quoted value survives only
// when it looks like a table or column name. Email addresses are removed
// everywhere.
func Redact(s string) string {
	s = quotedRe.ReplaceAllStringFunc(s, func(q string) string {
		if identRe.MatchString(q) && !strings.Contains(q, "@") {
			return q
		}
		return "'…'"
	})
	return emailRe.ReplaceAllString(s, "<email>")
}
