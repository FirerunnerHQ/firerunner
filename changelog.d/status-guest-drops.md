### Added

- `firerunner status` reports what the guest firewall dropped since the
  network was applied, by reason: past the connection limit or rate, to a
  non-public address, or with a spoofed source. In `--json` it is
  `network.drops`, or `network.drops_error` when the counters cannot be read
  (they need root).
