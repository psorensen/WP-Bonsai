# Bonsai

Bonsai turns a production WordPress database of any size into a small, scrubbed copy that still looks like production.

A 27 GB multisite newspaper network became a 46 MB dump in under 6 minutes. That is less than 0.2% of the original. The home page, menus, archives, and templates of every site still work.

## Why Bonsai

Most of a large WordPress database is content nobody opens during development: old articles, revisions, logs, caches, and form entries. A developer needs the parts that make the site look and behave like production. They do not need ten years of archive.

Bonsai keeps those parts and drops the rest:

- **The home page and site settings are kept.** The front page, the posts page, sticky posts, and every option except transients come along.
- **Menus are kept in full**, and so is every page or post a menu links to.
- **Every term is kept**, so each category and tag archive URL resolves. Each archive gets enough recent posts to show a second page.
- **Templates, template parts, global styles, reusable blocks, and ACF field definitions are always kept.**
- **Kept posts bring what they depend on:** featured images, images and files in their blocks, parent pages, related posts named in meta, and ACF image, gallery, and relationship fields.
- **Every post meta row of a kept post is kept, unchanged.** The size comes down by keeping fewer posts, never by trimming the posts that stay.

The result is a site that matches production where developers look: the home page, navigation, archives, and recent content. It leaves out the large body of posts that no page on the site surfaces.

## What gets dropped

- Older posts beyond what each post type's rule keeps, unless a kept post or menu depends on them
- Revisions, auto-drafts, oEmbed caches, and customizer changesets
- Comments, on every site
- Rebuildable indexes, such as Yoast indexables and WooCommerce lookup tables. Rebuild them after import.
- Logs, queues, and caches, such as Action Scheduler, Stream, Redirection logs, and Wordfence
- Personal data tables, such as orders, form entries, and newsletter lists
- Sites of a network that you exclude, and tables left over from deleted sites

## Results

Measured on a laptop with Apple silicon. Details are in [docs/benchmarks.md](docs/benchmarks.md).

| | |
| --- | --- |
| Input | 26.8 GB of SQL (6.9 GB gzip), 6 sites, 91.5 million rows |
| Output | 46 MB (5 MB gzip), scrubbed and validated |
| Time | 5 min 39 s from scratch; 2 min 18 s when the index is reused |
| Peak memory | 684 MB |

## How it works

1. **Pass 1** streams the dump once and writes a small DuckDB index: IDs, types, dates, sizes, and references between posts. It stores no post content and no personal data.
2. **Plan** reads your config and picks which rows to keep, with SQL on the index. It then estimates the output size, table by table.
3. **Pass 2** streams the dump again and writes only the kept rows.
4. **Sandbox finish** imports the result into a throwaway MariaDB and WP-CLI pair in Docker, on a network with no internet access. There it scrubs personal data, adds a local admin login, recounts terms, and validates the result. Only then does it export the final file.

Memory stays flat whatever the dump size. Both passes stream, and DuckDB's memory is capped.

## Requirements

- Go 1.27 or later, to build
- Docker, for the sandbox finish step
- An internet connection the first time only, to build the sandbox image

## Quick start

```sh
go build -o bonsai ./cmd/bonsai

# Pass 1: index the dump. Accepts .sql and .sql.gz files.
bonsai index production.sql.gz -out work/

# See what the dump holds: sites, post types, taxonomies, and tables.
bonsai inspect work/

# Try a config and see the size estimate. Writes nothing.
bonsai plan work/ -config bonsai.yml

# Build the scrubbed dump.
bonsai build production.sql.gz -config bonsai.yml -work work/ -out slim.sql
```

`build` writes `slim.sql` and a validation report, `slim.report.json`, next to it. Log in to the imported site as `bonsai` with the password `bonsai`.

`build` deletes the index when it finishes. Pass `-keep-work` to keep it for more `plan` runs.

## Configuration

Each project keeps one `bonsai.yml`, so every developer on the team gets the same dump. Anything you leave out uses a default.

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

Post type modes:

| Mode | Keeps |
| --- | --- |
| `latest` | The newest `count` posts |
| `per_term` | The newest `per_term` posts in each term of the listed taxonomies |
| `all` | Every post of the type |
| `none` | Only posts another kept post depends on |

A post type that the config does not list keeps its 10 newest published posts. `bonsai plan` lists every such type, so you can decide.

## Safety

- **The raw dump never leaves your machine.** Pass 1, pass 2, and the sandbox all run locally.
- **Scrubbing.** The sandbox runs 10up WP Scrubber (`wp scrub all`), which replaces every user's login, email, name, and password. Bonsai then replaces site and network admin emails.
- **The validation gate.** Bonsai checks every email and IP column of every table, and the references between posts, menus, and terms. If the scrubber fails or any check fails, Bonsai writes no output.
- **No GPL code in Bonsai.** The scrubber runs inside the sandbox image and is called only through WP-CLI.

## After importing

Run the rebuild commands the report lists, with the project's plugins active. For example:

```sh
wp yoast index
```

Images load from production through your usual local proxy. Bonsai handles database rows only, not media files.

## Status

Phase 1, the local CLI, is complete. It covers single sites and multisite networks. The SPEC.md phases that follow are a local web UI, then a hosted service, then a render test.

Known limits:

- Dumps must be `mysqldump`-style `.sql` or `.sql.gz` files. `.sql.zst`, `.zip`, and mydumper exports are not supported yet.
- WPML's `icl_translations` table is kept whole rather than filtered.
- Pass 1 reads gzip on one core. Decompressing the file first makes pass 1 faster.

## Development

```sh
go test ./...                     # unit tests
BONSAI_MARIADB=1 go test ./...    # adds the Docker tests: MariaDB round trips and the sandbox
go run ./cmd/synthdump -posts 5000 -subsite -o synthetic.sql   # a synthetic test dump
```

The design is in [SPEC.md](SPEC.md). Working rules for contributors are in [CLAUDE.md](CLAUDE.md).
