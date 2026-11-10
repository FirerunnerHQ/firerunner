### Security

- `build-toolcache.sh --python` relocates Python in a container with no
  network. The pip upgrade in python-versions' `setup.sh` now reinstalls the
  pip wheel bundled in the checked archive instead of fetching one from PyPI,
  so the drive holds only what was checked.
