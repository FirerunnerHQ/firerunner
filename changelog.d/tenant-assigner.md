### Added

- Daemons sharing a tenants list under `--tenant-leases` now spread the
  installations among themselves. One of them assigns each installation's
  account to the live daemon with the fewest tiers, at most 500 tiers a
  daemon, and moves it only when its daemon stops or stops beating. An
  installation listing more than 40 tiers is not served, and `firerunner
  status` and `doctor` say so on the assigning daemon. A daemon that stops
  hands its installations to the others at once.
