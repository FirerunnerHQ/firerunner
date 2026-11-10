### Security

- `--url` / `FR_URL` must be an https URL naming a GitHub organization,
  repository or enterprise; the scale-set client sends the configured
  credentials to it.
- The `build-image` workflow and the dispatched `e2e` job, which run on
  persistent self-hosted hosts, run only from `main`.
