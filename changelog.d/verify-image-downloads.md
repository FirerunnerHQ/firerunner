### Security

- `images/build-rootfs.sh` checks the `actions/runner` tarball against the
  SHA-256 in its GitHub release notes, and Node.js against the release's
  `SHASUMS256.txt`, before using them; a mismatch fails the build. It prints
  the version and checksums it used and keeps them in the image at
  `/etc/firerunner/downloads.sha256`. `--runner-sha256` and `--node-sha256`
  pin the checksums.
- `images/build-ubuntu-rootfs.sh` checks the same downloads, and Microsoft's
  `packages-microsoft-prod.deb` against a pinned SHA-256 (`--pmc-sha256`),
  on the host before the image build copies them in. `--toolset full` fetches
  actions/runner-images at a commit instead of a tag that can move: the
  default tag comes with its pinned commit, another `--runner-images-ref` is
  resolved to one and printed, and `--runner-images-commit` pins it.
- `images/build-toolcache.sh` checks Node.js against `SHASUMS256.txt`, Go
  against the go.dev download index, and Python and CodeQL against the
  SHA-256 the GitHub API reports for the release asset. `--checksums FILE`
  pins them, and each drive records its checksums in `/downloads.sha256`.
  `--go` and `--python` now need `jq`.
- `images/build-ec2-ami.sh` reads the runner's SHA-256 from its release
  notes (or `--runner-sha256`) before launching the build instance, which
  checks the download against it. runner-images is fetched at a commit:
  pinned for the default `--runner-images-ref`, resolved and printed for
  another, or given with `--runner-images-commit`. PowerShell modules are
  checked against the SHA-512 the PowerShell Gallery lists. A failed check
  fails the build, and the JSON output adds `runner_sha256`.
