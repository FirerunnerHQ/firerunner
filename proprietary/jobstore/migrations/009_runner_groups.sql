-- What keeping an installation's runner group last came to (#170): ready
-- or unavailable and why, the repositories served and the public ones
-- left out, as onboarding.GroupResult. Empty until first kept.
ALTER TABLE installations
    ADD COLUMN group_result   jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN group_synced_at timestamptz;
