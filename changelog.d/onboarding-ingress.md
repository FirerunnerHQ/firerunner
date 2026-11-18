### Added

- `deploy/cloudformation/onboarding.yaml`: the ingress for hosted onboarding. An API Gateway endpoint puts the GitHub App's webhooks on an SQS queue, which has a dead-letter queue; the endpoint runs no code and holds no secret. `control.yaml` takes `OnboardQueueUrl`, `OnboardQueueArn` and `OnboardWebhookSecretArn`. With all three, the control VM may read that queue and that secret, and the daemon applies the webhooks.
