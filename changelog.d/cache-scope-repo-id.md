### Fixed

- Cache entries are scoped by GitHub's repository ID as well as its name, so a
  repository created later under a name another one gave up reads none of the
  earlier one's caches. Entries saved before this version are no longer
  restored, and age out under the size and entry caps; the first jobs after an
  upgrade miss the cache once, as do a repository's first jobs after it is
  renamed or transferred. Upgrade the daemon and cache-server together.
