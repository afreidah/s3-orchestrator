This guide shows how to use the S3 Orchestrator from common S3 clients and SDKs. The orchestrator is a standard S3-compatible endpoint — any tool that speaks the S3 protocol will work.

## Prerequisites

You need four pieces of information from your orchestrator admin:

| Setting | Example |
|---------|---------|
| **Endpoint URL** | `http://s3-orchestrator.service.consul:9000` |
| **Bucket name** | `app1-files` |
| **Access Key ID** | `AKID_APP1_WRITER` |
| **Secret Access Key** | `wJalrXUtnFEMI/K7MDENG+bPxRfi...` |

Your credentials are tied to a specific bucket. You can only access the bucket your credentials are authorized for.

## AWS CLI

### Setup

Create a named profile so your orchestrator credentials don't conflict with other AWS configurations:

```bash
aws configure --profile orchestrator
# AWS Access Key ID: AKID_APP1_WRITER
# AWS Secret Access Key: wJalrXUtnFEMI/K7MDENG+bPxRfi...
# Default region name: us-east-1
# Default output format: json
```

The region value doesn't matter — the orchestrator accepts any region in the SigV4 signature. Pick any valid region name.

For convenience, set an alias or shell function:

```bash
alias s3o='aws --profile orchestrator --endpoint-url http://s3-orchestrator.service.consul:9000'
```

### Upload a file

```bash
s3o s3 cp myfile.txt s3://app1-files/path/to/myfile.txt

# With custom metadata
s3o s3api put-object --bucket app1-files --key path/to/myfile.txt \
    --body myfile.txt --metadata '{"project":"acme","env":"prod"}'
```

### Download a file

```bash
s3o s3 cp s3://app1-files/path/to/myfile.txt ./myfile.txt
```

### List objects

```bash
# List top-level "directories"
s3o s3 ls s3://app1-files/

# List everything under a prefix
s3o s3 ls s3://app1-files/path/to/ --recursive

# Detailed listing with the s3api command
s3o s3api list-objects-v2 --bucket app1-files --prefix "photos/"
```

### Delete a file

```bash
s3o s3 rm s3://app1-files/path/to/myfile.txt

# Delete all files under a prefix (uses batch DeleteObjects internally)
s3o s3 rm s3://app1-files/old-backups/ --recursive
```

### Copy within the bucket

```bash
s3o s3 cp s3://app1-files/old/location.txt s3://app1-files/new/location.txt
```

Cross-bucket copies are not supported. Both source and destination must be in the same bucket.

By default a copy carries the source object's tags. Pass `--tagging-directive REPLACE` with `--tagging` to give the copy a different set.

### Tag an object

```bash
# Tag on upload
s3o s3api put-object --bucket app1-files --key report.pdf \
    --body report.pdf --tagging "team=infra&retain=30d"

# Replace the whole set on an object that already exists
s3o s3api put-object-tagging --bucket app1-files --key report.pdf \
    --tagging 'TagSet=[{Key=team,Value=infra},{Key=retain,Value=30d}]'

# Read it back
s3o s3api get-object-tagging --bucket app1-files --key report.pdf

# Remove every tag
s3o s3api delete-object-tagging --bucket app1-files --key report.pdf
```

Tags describe the object, so all of its replicas share one set. `put-object-tagging` replaces the whole set rather than merging into it, and overwriting the key without `--tagging` leaves the new object untagged. Up to 10 tags per object. A `get-object` or `head-object` reports how many tags the object carries in `x-amz-tagging-count`, so a client can skip fetching a set that is not there. See [Object Tagging](tagging.md).

### Sync a directory

Upload an entire directory, only transferring new or changed files:

```bash
s3o s3 sync ./local-dir/ s3://app1-files/backup/

# Download direction
s3o s3 sync s3://app1-files/backup/ ./local-dir/

# With delete (remove files from destination that don't exist in source)
s3o s3 sync ./local-dir/ s3://app1-files/backup/ --delete
```

### Large file uploads

The AWS CLI automatically uses multipart upload for files larger than 8 MB. You can adjust the threshold:

```bash
aws configure set s3.multipart_threshold 64MB --profile orchestrator
aws configure set s3.multipart_chunksize 16MB --profile orchestrator
```

No special configuration is needed — multipart uploads work transparently.

## rclone

### Setup

Run `rclone config` and create a new remote:

```
n) New remote
name> orchestrator
Storage> s3
provider> Other
env_auth> false
access_key_id> AKID_APP1_WRITER
secret_access_key> wJalrXUtnFEMI/K7MDENG+bPxRfi...
region> us-east-1
endpoint> http://s3-orchestrator.service.consul:9000
```

Or add directly to `~/.config/rclone/rclone.conf`:

```ini
[orchestrator]
type = s3
provider = Other
access_key_id = AKID_APP1_WRITER
secret_access_key = wJalrXUtnFEMI/K7MDENG+bPxRfi...
endpoint = http://s3-orchestrator.service.consul:9000
region = us-east-1
```

### Usage

```bash
# Upload a file
rclone copy myfile.txt orchestrator:app1-files/path/to/

# Download a file
rclone copy orchestrator:app1-files/path/to/myfile.txt ./

# List objects
rclone ls orchestrator:app1-files/
rclone lsd orchestrator:app1-files/  # directories only

# Sync a directory
rclone sync ./local-dir/ orchestrator:app1-files/backup/

# Check what would change (dry run)
rclone sync ./local-dir/ orchestrator:app1-files/backup/ --dry-run
```

## Python (boto3)

### Setup

```bash
pip install boto3
```

### Client configuration

```python
import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="http://s3-orchestrator.service.consul:9000",
    aws_access_key_id="AKID_APP1_WRITER",
    aws_secret_access_key="wJalrXUtnFEMI/K7MDENG+bPxRfi...",
    region_name="us-east-1",
)
```

### Upload

```python
# From a file
s3.upload_file("myfile.txt", "app1-files", "path/to/myfile.txt")

# From bytes
s3.put_object(
    Bucket="app1-files",
    Key="path/to/data.json",
    Body=b'{"key": "value"}',
    ContentType="application/json",
    Metadata={"project": "acme", "env": "prod"},
)
```

### Download

```python
# To a file
s3.download_file("app1-files", "path/to/myfile.txt", "myfile.txt")

# To memory
response = s3.get_object(Bucket="app1-files", Key="path/to/data.json")
data = response["Body"].read()
```

### List objects

```python
response = s3.list_objects_v2(Bucket="app1-files", Prefix="photos/")
for obj in response.get("Contents", []):
    print(f"{obj['Key']}  ({obj['Size']} bytes)")

# Paginate large listings
paginator = s3.get_paginator("list_objects_v2")
for page in paginator.paginate(Bucket="app1-files", Prefix="photos/"):
    for obj in page.get("Contents", []):
        print(obj["Key"])
```

### Delete

```python
s3.delete_object(Bucket="app1-files", Key="path/to/myfile.txt")
```

### Copy

```python
s3.copy_object(
    Bucket="app1-files",
    Key="new/location.txt",
    CopySource="app1-files/old/location.txt",
)
```

### Tag an object

```python
# Tag on upload, query-string encoded
s3.put_object(
    Bucket="app1-files",
    Key="report.pdf",
    Body=b"...",
    Tagging="team=infra&retain=30d",
)

# Replace the whole set on an object that already exists
s3.put_object_tagging(
    Bucket="app1-files",
    Key="report.pdf",
    Tagging={"TagSet": [
        {"Key": "team", "Value": "infra"},
        {"Key": "retain", "Value": "30d"},
    ]},
)

response = s3.get_object_tagging(Bucket="app1-files", Key="report.pdf")
for tag in response["TagSet"]:
    print(f"{tag['Key']}={tag['Value']}")

s3.delete_object_tagging(Bucket="app1-files", Key="report.pdf")
```

See [Object Tagging](tagging.md) for the limits and the replacement semantics.

## Go (AWS SDK v2)

### Setup

```bash
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/aws/aws-sdk-go-v2/credentials
```

### Client configuration

```go
package main

import (
    "context"
    "fmt"
    "strings"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

func newClient() *s3.Client {
    return s3.New(s3.Options{
        BaseEndpoint: aws.String("http://s3-orchestrator.service.consul:9000"),
        Region:       "us-east-1",
        Credentials:  credentials.NewStaticCredentialsProvider("AKID_APP1_WRITER", "wJalrXUtnFEMI/K7MDENG+bPxRfi...", ""),
        UsePathStyle: true,
    })
}
```

### Upload

```go
client := newClient()
_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
    Bucket:      aws.String("app1-files"),
    Key:         aws.String("path/to/data.txt"),
    Body:        strings.NewReader("hello world"),
    ContentType: aws.String("text/plain"),
    Metadata:    map[string]string{"project": "acme", "env": "prod"},
})
if err != nil {
    fmt.Printf("upload failed: %v\n", err)
}
```

### Download

```go
result, err := client.GetObject(context.Background(), &s3.GetObjectInput{
    Bucket: aws.String("app1-files"),
    Key:    aws.String("path/to/data.txt"),
})
if err != nil {
    fmt.Printf("download failed: %v\n", err)
}
defer result.Body.Close()
// read result.Body
// user metadata is in result.Metadata (map[string]string)
```

### List objects

```go
result, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
    Bucket: aws.String("app1-files"),
    Prefix: aws.String("photos/"),
})
if err != nil {
    fmt.Printf("list failed: %v\n", err)
}
for _, obj := range result.Contents {
    fmt.Printf("%s  (%d bytes)\n", *obj.Key, *obj.Size)
}
```

### Delete

```go
_, err := client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
    Bucket: aws.String("app1-files"),
    Key:    aws.String("path/to/data.txt"),
})
```

### Tag an object

```go
// Tag on upload, query-string encoded
_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
    Bucket:  aws.String("app1-files"),
    Key:     aws.String("report.pdf"),
    Body:    strings.NewReader("..."),
    Tagging: aws.String("team=infra&retain=30d"),
})

// Replace the whole set on an object that already exists
_, err = client.PutObjectTagging(context.Background(), &s3.PutObjectTaggingInput{
    Bucket: aws.String("app1-files"),
    Key:    aws.String("report.pdf"),
    Tagging: &types.Tagging{TagSet: []types.Tag{
        {Key: aws.String("team"), Value: aws.String("infra")},
        {Key: aws.String("retain"), Value: aws.String("30d")},
    }},
})

result, err := client.GetObjectTagging(context.Background(), &s3.GetObjectTaggingInput{
    Bucket: aws.String("app1-files"),
    Key:    aws.String("report.pdf"),
})
for _, tag := range result.TagSet {
    fmt.Printf("%s=%s\n", *tag.Key, *tag.Value)
}
```

`types` is `github.com/aws/aws-sdk-go-v2/service/s3/types`. See [Object Tagging](tagging.md) for the limits and the replacement semantics.

## ETags

An object's ETag is the MD5 of the bytes you uploaded, in the same form S3 reports: the quoted hex digest for a single-part upload, and the MD5 of the concatenated part digests with a `-N` part-count suffix for a multipart one. Comparing it against the MD5 of your local file works, and comparing an object against itself over time works whichever replica answers the read.

That holds with compression or encryption enabled. The bytes on a backend are then not your bytes, and the ETag that backend would report is a digest of the stored form; the orchestrator reports the object's own value instead.

Two cases carry an ETag it did not compute:

- Objects imported by reconcile — bytes found on a backend with no ledger row — report what the backend reports until something reads them, at which point that value is recorded for every copy so it stops changing across a failover.
- Objects written before this version keep their backend value the same way. Compressed and encrypted ones get a computed ETag when the integrity scrubber next reads them, since only a plaintext read can produce one.

## Conditional Writes

The orchestrator honors the `If-None-Match: *` header on `PutObject` and `CompleteMultipartUpload` to give clients opt-in conflict detection. When the header is set and an object already exists at the target key, the request fails with `412 Precondition Failed` and the upload bytes are not stored.

The check is best-effort: a small race window exists between the existence check and the metadata commit, so two simultaneous writers each sending `If-None-Match: *` can both succeed in rare cases. This matches AWS S3's documented behavior — the precondition is a strong signal but not a hard guarantee under contention.

Only the `*` form is honored on writes. A specific etag value in `If-None-Match` is ignored on PUT and the upload proceeds as a normal overwrite.

**AWS CLI:**

```bash
aws s3api put-object --bucket app1-files --key new.txt --body new.txt \
    --if-none-match '*'
```

**Python (boto3):**

```python
s3.put_object(
    Bucket="app1-files",
    Key="new.txt",
    Body=b"contents",
    IfNoneMatch="*",
)
```

**curl:**

```bash
curl -X PUT -H 'If-None-Match: *' --data-binary @new.txt \
    http://s3-orchestrator.service.consul:9000/app1-files/new.txt
```

A `412 Precondition Failed` response can be retried with a different key or by intentionally omitting the header to overwrite.

## Presigned URLs

Presigned URLs let you generate a time-limited URL that grants temporary access to an object without requiring the requester to have credentials. Any AWS SDK presign client works (Go, Python boto3, Java, JavaScript, etc.).

### Generating a presigned URL

**AWS CLI:**

```bash
# Generate a presigned GET URL valid for 5 minutes (300 seconds)
s3o s3 presign s3://app1-files/path/to/myfile.txt --expires-in 300
```

**Python (boto3):**

```python
url = s3.generate_presigned_url(
    "get_object",
    Params={"Bucket": "app1-files", "Key": "path/to/myfile.txt"},
    ExpiresIn=300,
)
```

**Go (AWS SDK v2):**

```go
presignClient := s3.NewPresignClient(client)
req, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
    Bucket: aws.String("app1-files"),
    Key:    aws.String("path/to/myfile.txt"),
}, s3.WithPresignExpires(5*time.Minute))
// req.URL contains the presigned URL
```

### Using a presigned URL

The presigned URL can be used with any HTTP client — no AWS credentials or SDK required:

```bash
curl -o myfile.txt "THE_PRESIGNED_URL"
```

### Notes

- Presigned URLs use the same `access_key_id` and `secret_access_key` as normal requests. No additional configuration is needed.
- Maximum expiry is 7 days (604800 seconds). The server rejects URLs with a longer expiry.
- Presigned URLs work for GET, PUT, DELETE, and HEAD operations.
- For security recommendations (TLS, expiry values), see the [Security Hardening](security-hardening.md#presigned-url-security) guide.

## Request Tracing

Every response from the orchestrator includes an `X-Amz-Request-Id` header with a unique ID for that request. When reporting issues to your admin, include this ID so they can look up the full request trace in the audit logs.

You can also supply your own correlation ID by sending an `X-Request-Id` header with your request. The orchestrator will use your ID instead of generating one and return it in the response.

```bash
# Check the request ID in a response
s3o s3api put-object --bucket app1-files --key test.txt --body test.txt 2>&1 | grep -i request-id

# Supply your own correlation ID
curl -H "X-Request-Id: my-trace-123" \
     http://s3-orchestrator.service.consul:9000/app1-files/test.txt
# Response header: X-Amz-Request-Id: my-trace-123
```

## Limitations

The orchestrator implements a practical subset of the S3 API. A few things to be aware of:

- **Same-bucket copies only** — `CopyObject` requires source and destination to be in the same bucket.
- **No bucket management** — Buckets are configured server-side. `CreateBucket`, `DeleteBucket`, and `ListBuckets` are not supported.
- **No ACLs or policies** — Access control is handled entirely through the credential-to-bucket mapping in the server config.
- **No object versioning** — Each key holds exactly one object. Uploading to an existing key overwrites it. Concurrent PUTs to the same key are last-writer-wins, matching native S3 semantics. The orchestrator's per-key advisory lock keeps location metadata consistent across replicas, and bytes from the losing writer are enqueued for backend cleanup. Clients that need conflict detection should send `If-None-Match: *` (see [Conditional Writes](#conditional-writes)).
- **Max object size** — Configurable server-side (default: 5 GB). For larger objects, use multipart upload (most clients do this automatically).
- **Multipart upload timeout** — Incomplete multipart uploads are automatically cleaned up after 24 hours.
- **Range reads** — `GET` requests with a `Range` header are supported and return `206 Partial Content`.
