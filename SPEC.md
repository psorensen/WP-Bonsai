# Bonsai: Database Downsizer Spec

Exported from the shared spec doc on 2026-10-06. The doc is the source of truth; update this file when the doc changes.

Bonsai takes a production WordPress SQL dump of any size and returns a small, scrubbed, fully working dump for local development. The developer chooses which content to keep. The tool keeps everything that content depends on.

## Problem and goals

Our teams share one database per project for local development. Some production databases are too large for that. One newspaper client has a 32 GB database because it has no archive system. A developer cannot import that in a reasonable time, and it should not sit on laptops with reader PII in it.

**Goals**

- Output under a target size per project. The default target is 10 MB uncompressed.
- The site works after import: home page, archives, single posts, menus, search, pagination, and the admin all load without errors or broken references.
- PII is scrubbed before anyone downloads the file, using the existing Fueled scrubber rules.
- One tool for every project. Each project keeps its own saved configuration, so every developer on the team gets the same export.
- The developer chooses the information architecture to keep: which post types, how many recent posts of each, and how to sample by taxonomy term.

**Non-goals**

- Media files. The tool handles database rows only. Local sites load images from production through a proxy, as they do today.
- Syncing changes back to production.
- Non-WordPress databases.

## Decisions made

| Decision | Choice | Why |
| --- | --- | --- |
| Processing engine | Stream the SQL file in two passes. No database server. | Importing 32 GB into MySQL takes hours. A streaming parser reads the file at disk speed and holds only row IDs in memory. |
| Where it runs | Local CLI first. Hosted internal service in a later phase. | Local first proves the engine with no hosting, auth, or storage work. Hosting later gives one place to enforce scrubbing, auth, and retention. |
| Unknown plugin tables and references | Safe defaults, with per-table overrides | Known WordPress links are followed automatically. Log, cache, and analytics tables are emptied. The developer can change any table's rule. |
| Deliverable | Spec first, then build | The team reviews keep-set rules before code exists. |

The streaming engine has one cost. It cannot query the raw dump directly. Pass 1 solves this by writing a small DuckDB index, and the selection rules query that index instead.

## Architecture

The heavy work happens in two streaming passes over the raw file. Everything after that works on the index or on a file of about 10 MB, so it is cheap.

```
Upload / local path -> Pass 1: index (full dump) -> Configure -> Keep-set builder
                                                                      |
          Deliver <- Sandbox finish (scrub, recount, validate) <- Pass 2: write (full dump)
```

1. **Upload.** The browser uploads the dump straight to object storage in chunks, with resume. Accepted formats: `.sql`, `.sql.gz`, `.sql.zst`, `.zip`. A server-side pull from S3 or a host backup URL is also supported, because a 32 GB browser upload is fragile.
2. **Pass 1: index.** A Go parser reads every `CREATE TABLE` and `INSERT` statement. It writes a compact index to an embedded DuckDB file: post IDs, types, statuses, dates, parents, row byte sizes, term relationships, and every meta value that may point at another post. It also detects the table prefix, multisite tables, and ACF field definitions. Pass 1 stores no post content.
3. **Inventory and configure.** The UI reads the index and shows post types, taxonomies, and tables with counts and sizes. The developer edits the project config, or reuses the saved one.
4. **Keep-set builder.** SQL queries against the DuckDB index produce one ID list per table. Because the index is a real database, selection rules can be ordinary SQL. The builder also estimates the output size from stored row sizes before anything is written.
5. **Pass 2: write.** The parser streams the file again. It copies schema statements as they are and writes only rows whose IDs are in the keep set. It rewrites large multi-row inserts into batches of about 1 MB.
6. **Finish in a sandbox.** The slim dump is small, so the tool imports it into a throwaway MariaDB container with WP-CLI. There it runs the Fueled PII scrubber, recounts terms, clears dangling option references, and runs the validation checks. Then it exports the final dump.
7. **Deliver.** The scrubbed dump and a validation report are stored with the project. Download links expire. The raw upload is deleted on a schedule.

The engine is a single CLI binary. In phases 1 and 2, developers run it locally, and step 1 is a local file path instead of an upload. The hosted service in phase 3 is a web UI and job queue around the same binary. The same binary can run inside a client's own infrastructure when a contract forbids production data leaving it.

## Keep-set rules

The builder works in three steps. First it picks seed posts from the developer's choices. Then it adds every post those seeds depend on. Last, it filters every other table by the final post and user lists.

### Step 1: seed posts

Each post type gets one selection mode.

| Mode | What it keeps | Typical use |
| --- | --- | --- |
| Latest N | The N newest posts by `post_date` | Products: latest 100 |
| Per term | The K newest posts in each term of the chosen taxonomies | Articles: 20 per category, so each archive has more than one page |
| All | Every post of the type | Pages, forms, small config types |
| None | Nothing, unless another kept post depends on it | Revisions, old landing pages |

Per-term mode adds two guards. A newspaper can have 100,000 tags, so the developer sets a term cap per taxonomy, such as the 50 most used tags. The UI also reads `posts_per_page` from the options table and warns when K is not larger than it, because pagination then cannot be tested.

Status filter: published by default. The developer can add a few drafts, scheduled, or private posts per type to test editorial screens. Custom statuses work the same way. Pass 1 records every distinct `post_status` value per post type, including custom ones such as PublishPress or Edit Flow statuses. The inventory lists each status with its count, and the config accepts any status by name. Custom statuses are off by default. Edit Flow and PublishPress store their status definitions as terms, so keeping all terms keeps those definitions too.

Some post types are always kept in full because the site breaks without them: `nav_menu_item`, `wp_navigation`, `wp_template`, `wp_template_part`, `wp_global_styles`, `wp_block`, `wp_font_family`, `wp_font_face`, `custom_css`, and the ACF types `acf-field-group`, `acf-field`, `acf-post-type`, `acf-taxonomy`. Some are always dropped: `revision`, `customize_changeset`, `oembed_cache`, and auto-drafts.

### Step 2: follow dependencies

The builder repeats this step until no new posts are added, up to a set depth. The default depth is 2, which stops one related article from pulling in the whole archive.

| Reference | Where the ID lives |
| --- | --- |
| Parent pages and variations | `post_parent` |
| Featured image | `_thumbnail_id` meta |
| ACF image, file, gallery, relationship, post object, page link | Meta values, typed by the ACF field definitions found in pass 1, including serialized arrays |
| Block references | Block attributes in `post_content`: image IDs, reusable block `ref`, navigation `ref`, gallery IDs |
| Menu targets | `_menu_item_object_id` meta on menu items |
| Site settings | `page_on_front`, `page_for_posts`, `sticky_posts`, WooCommerce shop and cart page options |
| WooCommerce links | Upsell, cross-sell, and grouped product meta |

A reference to a post that does not exist in the dump is logged, not fatal. Plugin rules can add more reference patterns, for example Elementor JSON.

Note for pass 1: block references live in `post_content`, which pass 1 does not store. Pass 1 must extract the referenced IDs from `post_content` while streaming and store only the IDs.

### Step 3: filter the other tables

| Table | Rule |
| --- | --- |
| `postmeta` | All rows for kept posts, minus noise keys: `_edit_lock`, `_edit_last`, `_encloseme`, `_pingme`, `_oembed_*` |
| `terms`, `term_taxonomy`, `termmeta` | Keep all terms so every archive URL resolves. For flat taxonomies over a set size, keep only terms with kept posts. Always keep ancestors of kept terms. |
| `term_relationships` | Only rows for kept posts |
| `users`, `usermeta` | Authors of kept posts plus administrators, then scrubbed. The scrubber adds a known local admin login. |
| `comments`, `commentmeta` | Latest M approved comments per kept post. Default M is 3. Scrubbed. |
| `options` | All rows except transients. Any option over 100 KB is flagged in the size report. |
| `links` | All rows |
| Plugin and unknown tables | See the table rules below |

### Plugin and unknown table rules

The tool ships a rule library for common plugins. The developer can override any rule per project.

| Table type | Default |
| --- | --- |
| Rebuildable indexes: Yoast indexables, WooCommerce lookup tables | Empty. A post-import WP-CLI command rebuilds them. |
| Logs, queues, caches: Action Scheduler, Redirection logs, Wordfence, Stream | Empty |
| Personal data: WooCommerce orders and customers, Gravity Forms entries, newsletter lists | Empty |
| Configuration: Gravity Forms forms, Redirection rules | Keep all |
| Translation maps: WPML `icl_translations`, Polylang | Filter by kept post and term IDs |
| Unknown table with a `post_id` or `object_id` column | Filter by kept post IDs |
| Unknown table under 1 MB | Keep all |
| Unknown table over 1 MB | Empty, with a warning in the report |

## Configuration schema

Each project has one YAML config. The UI edits it, and the CLI reads the same file, so a team can also commit it to the project repo. Anything left out uses the defaults above.

```yaml
project: example-newspaper
target_size_mb: 10
dependency_depth: 2

post_types:
  post:
    mode: per_term
    per_term: 20
    taxonomies:
      category: { max_terms: all }
      post_tag: { max_terms: 50, order: most_used }
    statuses: { publish: all, draft: 3, future: 2, pitch: 2 }
  page:
    mode: all
  product:
    mode: latest
    count: 100
  obituary:
    mode: latest
    count: 25
  ad_campaign:
    mode: none

taxonomies:
  prune_unused_flat_terms_over: 500

comments:
  per_post: 3

users:
  include_roles: [administrator]

meta:
  exclude_keys: ["_edit_lock", "_edit_last", "_oembed_*", "legacy_import_*"]

tables:
  wp_custom_paywall_log: empty
  wp_custom_bylines: { filter_by: post_id }
  wp_custom_settings: keep

references:
  extra_meta_keys: [related_story_id, hero_video_id]

scrub:
  profile: fueled-default
```

## UI flow

The first run for a project needs configuration. Later runs reuse the saved config, so a refresh is upload, confirm, download. The local UI in phase 2 covers steps 3 to 5. Projects, upload, and notifications arrive with hosting in phase 3.

1. **Projects.** A list of client projects, each with its saved config, past snapshots, and access list.
2. **Upload.** Drop a file or paste a storage URL. A progress bar shows upload, then the pass 1 index.
3. **Inventory.** Three tabs, built from the index:
   - Post types: row count, date range, average row size, and total size.
   - Taxonomies: term count, the post types that use each taxonomy, and the largest terms.
   - Tables: size, row count, the rule that applies, and where the rule came from (core, plugin library, or unknown).
4. **Configure.** Each post type has a mode picker, with count and taxonomy settings. Each table has a rule picker. New post types or tables that are not in the saved config are highlighted.
5. **Size preview.** A live estimate updates as settings change. It shows a bar per table, so the developer can see what uses the space, for example a 4 MB options table. If the estimate is over the target, the screen names the three biggest contributors.
6. **Run.** Pass 2, the sandbox finish, and validation run as one job. The page shows progress and can be closed. A Slack or email message is sent when the job ends.
7. **Result.** The validation report, the final size, and a download link. The page also shows a one-line import command for the team's local setup, for example `wp db import` or the project's local-env script.

## Security and data handling

The hosted service holds raw client production data, including reader PII. It must be treated like a production system, not a dev tool. In the local phases, the raw dump sits on the developer's machine. The CLI deletes its index and scratch files after each run and reminds the developer to delete the raw file.

| Area | Requirement |
| --- | --- |
| Sign-in | Company SSO only. Access is limited to engineering staff. |
| Project access | Each project has a member list. A person sees only the projects they belong to. |
| Raw uploads | Encrypted at rest. Never downloadable. Deleted 72 hours after upload by default. Each project can set a shorter time. |
| Outputs | Only scrubbed outputs can be downloaded. Links are signed and expire after 24 hours. |
| Scrub gate | If the scrubber fails, the job fails. The tool never delivers an unscrubbed file. |
| Workers | Jobs run in isolated containers with no outbound internet. Scratch disks are wiped after each job. |
| Audit log | Every upload, config change, run, and download is logged with user and time. |
| Client contracts | Some contracts may forbid production data leaving client infrastructure. For those projects, the team runs the CLI inside the client's environment and uploads only the scrubbed output. |

## Validation

Every run ends with a report. A failed check marks the snapshot as failed, and a warning lets the download go ahead. The checks run in the sandbox after the scrub step.

| Check | Result if it fails |
| --- | --- |
| The dump imports into MariaDB with no errors | Fail |
| No term relationship, post meta, or comment points at a missing post | Fail |
| Every kept featured image and ACF image ID exists | Fail |
| Every menu item's target exists | Fail |
| `page_on_front`, `page_for_posts`, and the WooCommerce page options point at kept pages | Fail |
| No email address or IP address outside the scrubber's allowed patterns | Fail |
| Each per-term archive has more posts than `posts_per_page` | Warning |
| Output is under the target size | Warning, with the biggest tables listed |
| References to posts that were never in the dump | Warning, with a count |

Phase 4 adds a render test. The sandbox checks out the project's theme and plugins from the repo, then requests the home page, one archive per kept taxonomy, one single post per type, and page 2 of one archive. Any PHP fatal error or HTTP 500 fails the run.

## Risks

| Risk | Mitigation |
| --- | --- |
| Dump formats differ: mysqldump, WP-CLI, mydumper, host exports, odd escaping | Build a test corpus of real exports from each host we use. Fuzz the parser. |
| IDs hidden in serialized PHP or JSON, such as page builders | PHP unserialize support in pass 1. Per-plugin reference rules. Missing references show in the report. |
| 10 MB is not reachable on some sites, for example when options alone are 6 MB | The size report names the cause. The target is a warning, not a failure. |
| A dependency chain pulls in too much content | Depth limit, plus a report of which rule added the most posts. |
| The hosted service holds client PII | See the security section. Legal reviews it before the first client dump is uploaded. |

## Open questions

- [ ] Which Fueled scrubber do we package, and does it run on WP-CLI without the site's theme active?
- [ ] Does WP Snapshots already do part of this? If so, should Bonsai feed into it instead of replacing it?
- [ ] Is 10 MB the right default target, or should it be set per project?
- [ ] Do we need multisite in phase 1? Which current clients run multisite?
- [ ] Which client contracts restrict where production data can go?
- [ ] Who owns and hosts the service: the internal tools team or a project team?

## Phased plan

1. **Phase 1: local CLI.** Developers run the engine on their own machine against a dump file. It includes the two-pass parser, the DuckDB index, the keep-set builder, and a YAML config committed to the project repo. The sandbox step runs in local Docker with MariaDB and WP-CLI. Prove it on the 32 GB newspaper dump.
2. **Phase 2: local UI.** A command such as `bonsai ui` opens a local web page for the inventory, configure, and size preview steps. It reads and writes the same YAML file. Add the plugin rule library.
3. **Phase 3: hosted service.** Wrap the same engine in a shared service with upload, job queue, SSO, retention, and audit log. The security section applies from this phase.
4. **Phase 4: depth.** Render test with the project theme, multisite, and Slack notices.
