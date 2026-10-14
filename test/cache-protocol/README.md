# Cache protocol integration test

This runs the pinned real `@actions/cache` toolkit against the built executable.
It creates and extracts actual archives and checks default/base/PR precedence,
sibling isolation, PR reruns, and read-only behavior before a job record exists.
All cache files and host identity fixtures live in a disposable directory.

```sh
go build -o /tmp/firerunner-cache-test ./cmd/firerunner
npm ci --prefix test/cache-protocol --ignore-scripts
FIRERUNNER_BINARY=/tmp/firerunner-cache-test npm test --prefix test/cache-protocol
```

Requires Node 24, tar, and zstd. CI runs it on every PR. Host identity publication
is simulated here; resolver API fixtures and registry lifecycle tests live in
`internal/jobidentity`, real packet anti-spoof tests in the Linux `netns` suite.
This is not a real GitHub assignment/Firecracker boot test. Before rollout,
use a dedicated repository and an upgraded daemon/cache server to exercise a
push, a fork PR and a rerun with `actions/cache`, and verify that job identities
are published with the installation's actual permissions. An unavailable or
ambiguous GitHub ref must produce a denied save, never a trusted-scope write.
