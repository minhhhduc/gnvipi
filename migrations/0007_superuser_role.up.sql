-- Rename the owner role to superuser (#322).
--
-- Nothing is added or removed: root, superuser, admin and user keep the ranks
-- they had as root, owner, admin and user, and the last-active-account guard
-- still counts every account at or above the renamed role. The rows move in
-- the same transaction as the constraint, so a half-applied rename cannot exist.

-- 1. The guard has to recognise the new value before the rows move. Triggers
--    hold their function by OID, so renaming the function and the triggers
--    afterwards keeps both definitions wired to it.
CREATE OR REPLACE FUNCTION users_guard_last_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE remaining integer;
BEGIN
  -- Root rows are governed by users_protect_root; counting them here would
  -- refuse the promotion that makes a superuser root in the first place.
  IF OLD.role = 'root' THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    RETURN NEW;
  END IF;
  IF TG_OP = 'DELETE' THEN
    IF OLD.role = 'superuser' AND OLD.status = 'active' THEN
      PERFORM pg_advisory_xact_lock(hashtext('gnvipi:superusers'));
      SELECT count(*) INTO remaining FROM users
       WHERE role IN ('superuser', 'root') AND status = 'active' AND id <> OLD.id;
      IF remaining = 0 THEN
        RAISE EXCEPTION 'last active superuser cannot be deleted';
      END IF;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.role = 'superuser' AND OLD.status = 'active'
     AND (NEW.role <> 'superuser' OR NEW.status <> 'active') THEN
    PERFORM pg_advisory_xact_lock(hashtext('gnvipi:superusers'));
    SELECT count(*) INTO remaining FROM users
     WHERE role IN ('superuser', 'root') AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active superuser cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;

ALTER FUNCTION users_guard_last_owner() RENAME TO users_guard_last_superuser;
ALTER TRIGGER users_guard_last_owner_bu ON users RENAME TO users_guard_last_superuser_bu;
ALTER TRIGGER users_guard_last_owner_bd ON users RENAME TO users_guard_last_superuser_bd;

-- 2. Move the rows. The v6 check forbids 'superuser', so it is dropped for the
--    update and restored with the new vocabulary once every row validates.
ALTER TABLE users DROP CONSTRAINT users_role_check;
UPDATE users SET role = 'superuser' WHERE role = 'owner';
ALTER TABLE users ADD CONSTRAINT users_role_check
  CHECK (role IN ('root', 'superuser', 'admin', 'user'));