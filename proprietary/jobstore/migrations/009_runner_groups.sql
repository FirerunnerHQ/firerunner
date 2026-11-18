-- What keeping an installation's runner group last came to (#170): ready
-- or unavailable and why, the repositories served and the public ones
-- left out, as onboarding.GroupResult. Empty until first kept. group_id
-- is the group onboarding created in the organization, 0 before it has:
-- onboarding keeps that group alone, never another of the same name.
ALTER TABLE installations
    ADD COLUMN group_result    jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN group_synced_at timestamptz,
    ADD COLUMN group_id        bigint NOT NULL DEFAULT 0;
