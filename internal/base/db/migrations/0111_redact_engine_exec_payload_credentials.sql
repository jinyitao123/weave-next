UPDATE weave_task_queue
SET payload = payload - 'env' - 'oneapi_base' - 'oneapi_key'
WHERE kind = 'engine_exec'
  AND jsonb_typeof(payload) = 'object'
  AND (
    payload ? 'env'
    OR payload ? 'oneapi_base'
    OR payload ? 'oneapi_key'
  );
