# Bonsai

Bonsai shrinks a production WordPress database into a small, scrubbed copy that you can run locally. It keeps the parts of the site people actually see, so the local copy still looks and behaves like production.

In our largest test, a 27 GB multisite newspaper network came down to a 46 MB dump in under six minutes. Every site's home page, menus, archives, and templates still worked.

## Why Bonsai

Most of a large WordPress database is content you never open while developing. There are years of old articles, thousands of revisions, logs, caches, and form entries. Importing all of that takes hours, and it puts reader data on every laptop.

What you need locally is much smaller: the home page, the navigation, the archive pages, recent content, and the settings that hold the site together. Bonsai keeps exactly that.

It starts from the home page, the menus, and the most recent posts of each type. Then it follows everything those posts depend on: featured images, images and files in the content, parent pages, related posts, and ACF fields. It keeps every category and tag, and it gives each archive enough posts to show a second page. Templates, global styles, reusable blocks, and ACF field definitions always come along.

The result is a site that matches production wherever developers look, without the long tail of posts that nothing on the site links to. Bonsai never trims the posts it keeps. All of their post meta comes along unchanged, so any template that works in production works locally too.

## What Bonsai leaves out

- Older posts that no kept page, menu, or post depends on
- Revisions, auto-drafts, oEmbed caches, and customizer changesets
- Comments
- Indexes that plugins can rebuild, such as Yoast indexables and WooCommerce lookup tables
- Logs, queues, and caches from plugins such as Action Scheduler, Stream, Redirection, and Wordfence
- Personal data tables, such as orders, form entries, and newsletter lists
- Sites of a network that you choose to exclude, and tables left over from deleted sites

## Results

These numbers come from a laptop with Apple silicon. See [docs/benchmarks.md](docs/benchmarks.md) for the details.

| | |
| --- | --- |
| Input | 26.8 GB of SQL (6.9 GB gzipped), 6 sites, 91.5 million rows |
| Output | 46 MB (5 MB gzipped), scrubbed and validated |
| Time | 5 min 39 s from scratch, or 2 min 18 s when the index is reused |
| Peak memory | 684 MB |

## How it works

Bonsai reads the dump twice and never loads it into a database server until the very end.

1. **Index.** The first pass streams the dump and builds a small DuckDB index of IDs, post types, dates, sizes, and the references between posts. The index holds no post content and no personal data.
2. **Plan.** Bonsai applies your config to the index with SQL. It decides which rows to keep and estimates the size of the output, table by table, before writing anything.
3. **Write.** The second pass streams the dump again and writes only the rows in the plan.
4. **Finish.** Bonsai imports the result into a throwaway MariaDB and WP-CLI setup in Docker, with no internet access. There it scrubs personal data, adds a local admin login, recounts terms, and checks the result before exporting the final file.

Because both passes stream, memory use stays flat no matter how large the dump is.

## Requirements

You need Go 1.27 or later to build Bonsai, and Docker to run the finish step. The first build of the sandbox image downloads WordPress and the scrubber, so that one step needs an internet connection.

## Quick start

```sh
go build -o bonsai ./cmd/bonsai

# Index the dump. Bonsai reads .sql and .sql.gz files.
bonsai index production.sql.gz -out work/

# See what the dump holds: sites, post types, taxonomies, and tables.
bonsai inspect work/

# Try a config and check the size estimate. This writes nothing.
bonsai plan work/ -config bonsai.yml

# Build the scrubbed dump.
bonsai build production.sql.gz -config bonsai.yml -work work/ -out slim.sql
```

`build` writes `slim.sql` along with a validation report, `slim.report.json`. After you import the file, log in as `bonsai` with the password `bonsai`.

By default, `build` deletes the index when it finishes. Add `-keep-work` if you want to keep it for more `plan` runs.

## Configuration

Each project keeps a single `bonsai.yml`, so everyone on the team gets the same dump. You only need to set what differs from the defaults.

```yaml
project: example-newspaper
target_size_mb: 50

post_types:
  post:
    mode: per_term          # the newest posts in each term
    per_term: 11            # more than posts_per_page, so archives have a page 2
    taxonomies:
      category: { max_terms: all }
      post_tag: { max_terms: 50 }
    statuses: { publish: all, draft: 3 }
  page: { mode: all }
  product: { mode: latest, count: 100 }
  obituary: { mode: latest, count: 25 }
  ad_campaign: { mode: none }

references:
  # Meta keys that hold post IDs, in addition to the built-in ones.
  extra_meta_keys: [related_story_id, hero_video_id]

tables:
  wp_custom_paywall_log: empty
  wp_custom_bylines: { filter_by: post_id }

sites:                      # multisite only
  "*": { exclude: true }    # drop every site not listed below
  "1": { exclude: false }
  "3": { exclude: false, post_types: { post: { mode: latest, count: 50 } } }
```

Each post type uses one of four modes:

| Mode | What it keeps |
| --- | --- |
| `latest` | The newest `count` posts |
| `per_term` | The newest `per_term` posts in each term of the listed taxonomies |
| `all` | Every post of the type |
| `none` | Only the posts that other kept posts depend on |

A post type that isn't in the config keeps its 10 newest published posts. `bonsai plan` lists those types, so you can decide whether the default is right.

## Safety

The raw dump never leaves your machine. Every step runs locally, and the finish step runs in containers that cannot reach the internet.

To scrub personal data, the finish step runs 10up WP Scrubber (`wp scrub all`), which replaces every user's login, email, name, and password. Bonsai then replaces the admin emails of each site and of the network.

Before writing anything, Bonsai validates the result. It checks every email and IP address column in every table, as well as the references between posts, menus, and terms. If the scrubber fails or any check fails, Bonsai writes no output at all.

WP Scrubber is GPL-licensed, so it runs only inside the sandbox image, called through WP-CLI. None of its code is part of Bonsai.

## After importing

The report lists any commands that rebuild tables Bonsai emptied. Run them with the project's plugins active, for example:

```sh
wp yoast index
```

Bonsai handles database rows only. Images keep loading from production through your usual local proxy.

## Status

Phase 1, the local command-line tool, is complete. It handles single sites and multisite networks. The next phases in [SPEC.md](SPEC.md) are a local web UI, a hosted service, and a render test.

A few known limits:

- Bonsai reads `mysqldump`-style `.sql` and `.sql.gz` files. It doesn't support `.sql.zst`, `.zip`, or mydumper exports yet.
- WPML's `icl_translations` table is kept whole rather than filtered.
- Gzip decompression runs on a single core, so pass 1 is faster on a dump you decompress first.

## Development

```sh
go test ./...                     # unit tests
BONSAI_MARIADB=1 go test ./...    # adds the Docker tests: MariaDB round trips and the sandbox
go run ./cmd/synthdump -posts 5000 -subsite -o synthetic.sql   # generate a synthetic test dump
```

The design is in [SPEC.md](SPEC.md), and the working rules for contributors are in [CLAUDE.md](CLAUDE.md).
