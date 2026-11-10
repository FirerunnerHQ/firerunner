### Changed

- cache-server lets at most 16 requests from one address, and 1024 in all,
  wait on a pending job identity at a time; one more is denied at once, with
  the new reason `waiters` in `/stats` and `/metrics`.
