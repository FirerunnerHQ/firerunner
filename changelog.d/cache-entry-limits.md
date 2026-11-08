### Fixed

- The cache server bounds its number of entries, not only their bytes: a
  repository keeps at most `--max-repo-entries` (default 2000), evicting its
  own least-recently-used first, and the host at most `--max-entries` (default
  20000). A job may hold at most 64 unfinished uploads, and requests with more
  than ten keys or a key longer than 512 characters are refused, as
  `@actions/cache` does.
