### Added

- Daemons sharing a tenants list under `--tenant-leases` now spread the
  installations among themselves. One of them assigns each installation's
  account to the live daemon with the fewest tiers, at most 500 tiers a
  daemon, and moves it only when its daemon stops, stops beating, or fails
  to apply the tenants list. An installation listing more than 40 tiers is
  placed on no daemon. `firerunner status` and `doctor` say which daemon
  assigns, and on that one which installations are not placed and why. A
  daemon that stops hands its installations to the others at once. Upgrade
  every daemon together: an older one ignores the assignments.
