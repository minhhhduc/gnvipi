-- Allow promoting the sole active superuser to active root (#338).
--
-- users_guard_last_superuser() previously checked NEW.role <> 'superuser',
-- which treated a superuser -> root promotion as a demotion when no other
-- active superuser or root existed. Treat both superuser and root as active
-- privileged roles on the NEW side.

CREATE OR REPLACE FUNCTION users_guard_last_superuser() RETURNS trigger
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
     AND (NEW.role NOT IN ('superuser', 'root') OR NEW.status <> 'active') THEN
    PERFORM pg_advisory_xact_lock(hashtext('gnvipi:superusers'));
    SELECT count(*) INTO remaining FROM users
     WHERE role IN ('superuser', 'root') AND status = 'active' AND id <> OLD.id;
    IF remaining = 0 THEN
      RAISE EXCEPTION 'last active superuser cannot be demoted or disabled';
    END IF;
  END IF;
  RETURN NEW;
END $$;
