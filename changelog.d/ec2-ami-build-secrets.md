### Security

- `build-ec2-ami.sh` keeps a `--firerunner-url` out of the build's shell
  trace, and the build removes cloud-init's copies of its user-data and its
  logs before the image is taken, so a presigned URL does not end up in the
  runner image.
