### Added

- Entitlements have a `pending` account state: the installation is held, expired
  or not, with billing's reason (a payment method to add, say) until a new
  record lifts it. `firerunner entitlement set --account pending` writes one.
