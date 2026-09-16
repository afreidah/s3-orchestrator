# A credential is imported by its access key, which is also its id.
#
# The secret does not come with it: the orchestrator never reads one back out.
# State holds a null secret until the configuration supplies the one already
# held, which is another reason to supply a keypair rather than mint it.
terraform import s3orchestrator_credential.backup MFRGGZDFMZTWQ2LKNNWG
