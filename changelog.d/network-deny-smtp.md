### Security

- `network.yaml`'s runner subnets deny outbound SMTP (TCP 25, 465 and 587)
  through a network ACL, so job code on an EC2 runner cannot send mail; a
  security group can only allow. A VPC of your own needs the same.
