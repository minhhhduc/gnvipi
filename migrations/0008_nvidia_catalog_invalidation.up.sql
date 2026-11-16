-- Propagate NVIDIA catalog changes across instances (#330).
CREATE TRIGGER nvidia_models_bump_config
AFTER INSERT OR UPDATE OR DELETE ON nvidia_models
FOR EACH STATEMENT EXECUTE FUNCTION bump_config_version();
