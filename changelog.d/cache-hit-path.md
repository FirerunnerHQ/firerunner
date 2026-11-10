### Changed

- A cache hit no longer rewrites cache-server's whole index under its lock:
  the entry's last-used time is written within 30 s (plus the S3 sync
  interval for an S3 index), by any other change sooner, and on shutdown. A
  lookup also scans the entries once rather than once per ref and restore key.
