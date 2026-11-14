### Fixed

- Stopping or restarting firerunner no longer waits for EC2 runners that are
  running a job. Their instances keep running the job, and the next start's
  reaper ends each one once its job is done. Before, a stop waited for those
  jobs. On the CloudFormation control VM, a stop that ran past the unit's
  60-second limit failed the unit, and the VM replaced itself. A runner's
  start, if it reported one, is now recorded before it is left.
