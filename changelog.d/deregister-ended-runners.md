### Fixed

- When a runner's machine ends before the runner starts a job, firerunner now deregisters the runner at GitHub right away. Causes include a spot instance AWS takes back from a hot runner, a crash, or a boot that fails after registering.
  - A runner that had connected could otherwise still be assigned a job, which then waited minutes before GitHub gave it to another runner.
  - Before, only the offline-runner sweep (`--runner-sweep`) removed such runners, and later.
  - Removals run one at a time per tier and stop at shutdown.
