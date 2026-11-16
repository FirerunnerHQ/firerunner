### Added

- `--tenant-leases` lets several daemons share one `--tenants db:` list.
  Each serves an installation only while it holds the lease on its GitHub
  account in the job store's database, renewed every 10 seconds. A daemon
  that cannot renew stops launching for it within 20 seconds and hands it
  off: its sessions close, its busy runners finish, and another daemon takes
  it over once the lease expires (30 seconds). `firerunner status` shows the
  daemon's shard id and how many installations it holds, and
  `firerunner doctor` warns while renewals fail (`tenant-leases`).
- `--tenant-owner` sets the owner tag of the installations' instances,
  `<tenant-owner>/<id>`, apart from `--owner`. Daemons that share a list
  share it, each with an `--owner` of its own. It defaults to `--owner`, as
  before. `firerunner tenant findings` takes it too.
