### Fixed

- A runner whose machine ends before it takes a job is now deregistered at GitHub right away. This covers a spot instance AWS takes back from a hot runner, a crash, or a boot that fails after registering. GitHub could otherwise still assign that runner a job, which then waited minutes before going to another runner. Before, the offline-runner sweep removed such runners only later.
