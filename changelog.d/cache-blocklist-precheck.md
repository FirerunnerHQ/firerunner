### Fixed

- cache-server checks a block list before assembling it: a block named more
  than once, or one never uploaded, is refused with 400 before the upload
  claims any space, so a failing commit no longer evicts other entries.
