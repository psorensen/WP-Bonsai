# Benchmarks

Milestone 6: a full run on a large real dump. Client details are left out on purpose.

## Machine

Apple silicon laptop (macOS, 10 cores, 24 GB RAM), local SSD, Docker Desktop. Bonsai built with Go 1.27.1.

## Dump

| | |
| --- | --- |
| Source | `mysqldump` 8.4 export of a WordPress VIP multisite network, gzip-compressed |
| Size | 6.9 GB compressed, 26.8 GB of SQL |
| Content | 6 sites, 223 tables, 91.5 million rows, about 2.3 million posts and 4.6 million attachments |
| Largest table | `wp_10_posts`, 7.1 GB |

## Results

`bonsai build` from scratch, built-in defaults (no project config), output `slim.sql`:

| Phase | Time | Peak memory | Notes |
| --- | --- | --- | --- |
| Pass 1: index | 3 min 23 s | 668 MB | 136 MB/s of SQL. Limited by single-threaded gzip decompression and parsing. |
| Plan | under 1 s | 407 MB | Keep set and size estimate for all 6 sites. |
| Pass 2: write | 1 min 58 s | | 227 MB/s of SQL. Kept 66,851 rows. |
| Sandbox finish | 17 s | | Import, WP Scrubber, admin login, term recount, validation, export. |
| **Total** | **5 min 39 s** | **684 MB** | Output 45.9 MB. Estimate 45.9 MB. |

Peak memory is the `bonsai` process only. The sandbox containers run in Docker.

A second build that reuses the index takes 2 min 18 s at 421 MB.

## DuckDB memory cap

DuckDB's memory is capped by `BONSAI_DUCKDB_MEMORY`, 256MB by default. On this dump:

| Cap | Pass 1 time | Pass 1 peak | Plan time | Plan peak |
| --- | --- | --- | --- | --- |
| 1GB | 3 min 22 s | 1.7 GB | 1.5 s | 1.2 GB |
| 256MB | 3 min 24 s | 668 MB | 0.8 s | 407 MB |

The smaller cap costs no time, so it is the default.

## Synthetic dumps, pass 1 only

| Dump | Time | Peak memory |
| --- | --- | --- |
| 500 MB | 1 s | 130 MB |
| 2.5 GB | 6 s | 175 MB |

These ran before the cap was lowered to 256MB.
