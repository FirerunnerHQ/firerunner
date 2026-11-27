### Added

- `network.yaml` takes `SubnetCount=4`, for a fourth subnet in the region's
  fourth Availability Zone: more spot capacity, and one more zone's price for
  spot launches to pick the cheapest from. The VPC's address plan already
  held a fourth /18, so an existing stack only gains the subnet.
