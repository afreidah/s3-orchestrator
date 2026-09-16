# A user is imported by the id the orchestrator generated, which `s3o admin
# user list` prints. The name is not an identifier: a rename does not move the
# id, which is the whole reason credentials and grants reference it.
terraform import s3orchestrator_user.backup user-4kqx7n2mjb5tza6wfhc3
