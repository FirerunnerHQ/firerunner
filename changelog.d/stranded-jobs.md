### Fixed

- A job queued while a tier had no listener for more than about 4 minutes (a
  replaced control VM, a crash loop) no longer waits until the installation
  queues another job. GitHub cancels its assignment to the scale set and
  counts it nowhere, so firerunner now launches a runner for it, which GitHub
  hands the job to. The daemon logs `job stranded`, and `status` counts such
  jobs per tier (`stranded`, `rescuing`).
