### Security

- `deploy-control.sh` checks the build provenance of the release it deploys
  before it pins its checksums: it downloads the binaries and
  `firerunner-control-bootstrap.sh` and runs `gh attestation verify` on each,
  requiring the release workflow, the deployed tag and a GitHub-hosted runner.
  It now needs `gh`. Releases before v0.1.9 carry no attestations; set
  `DEPLOY_SKIP_ATTESTATION=1` to deploy one.
