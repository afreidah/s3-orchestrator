# A grant has no identifier of its own: the orchestrator keys it by user, kind
# and name together, so the import identifier is those three joined by a slash.
terraform import s3orchestrator_grant.photos user-4kqx7n2mjb5tza6wfhc3/bucket/photos

# A grant on the orchestrator takes no name, so its identifier ends in an empty
# third segment. The trailing slash is required rather than optional.
terraform import s3orchestrator_grant.operator user-4kqx7n2mjb5tza6wfhc3/orchestrator/
