### Changed

- Spot launches try each instance type's subnets cheapest zone first, by the
  current spot price, instead of always starting at the first subnet; zones
  within 3% of the cheapest take turns. Prices are read when a spot tier
  starts and every 5 minutes after, in the background. Without them, launches
  keep the configured order. The control
  VM's role needs `ec2:DescribeSpotPriceHistory`; the CloudFormation templates
  grant it, and `firerunner doctor` checks it (`spot-prices`).
- `instance launched` log lines for spot instances name `spotPrice`, the
  zone's spot price as last read.
