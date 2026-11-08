### Fixed

- Firecracker guests no longer reach private addresses through a host's own
  DNAT rules, such as Docker's or Kubernetes' published ports, in any egress
  mode. Only the host's redirect of guest DNS (TCP or UDP to port 53) is still
  exempt from the non-public drop, and packets conntrack marks invalid are
  dropped.
