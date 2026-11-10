### Changed

- The control VM's bootstrap reads `<prefix>/control/env` as data: blank
  lines, `#` comments and `FR_NAME=value` lines, with a value bare or in
  quotes. Any other line, or a value holding a backslash, `$` or a backtick,
  fails the bootstrap with the line's number in the log. Values reach
  firerunner exactly as written.
