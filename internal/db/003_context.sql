-- Context captured at record time: the Git branch and opt-in environment
-- values (a JSON object of configured variables). Existing rows remain NULL.
ALTER TABLE history ADD COLUMN branch TEXT;
ALTER TABLE history ADD COLUMN env TEXT;
PRAGMA user_version = 3;
