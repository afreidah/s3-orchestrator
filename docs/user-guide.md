# S3 Orchestrator User Guide

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
```

### Copy within the bucket

```bash
s3o s3 cp s3://app1-files/old/location.txt s3://app1-files/new/location.txt
```

Cross-bucket copies are not supported. Both source and destination must be in the same bucket.

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

## Limitations

The orchestrator implements a practical subset of the S3 API. A few things to be aware of:

- **Same-bucket copies only** — `CopyObject` requires source and destination to be in the same bucket.
- **No bucket management** — Buckets are configured server-side. `CreateBucket`, `DeleteBucket`, and `ListBuckets` are not supported.
- **No ACLs or policies** — Access control is handled entirely through the credential-to-bucket mapping in the server config.
- **No object versioning** — Each key holds exactly one object. Uploading to an existing key overwrites it.
- **No presigned URLs** — All requests must be signed at the time of the request.
- **Max object size** — Configurable server-side (default: 5 GB). For larger objects, use multipart upload (most clients do this automatically).
- **Multipart upload timeout** — Incomplete multipart uploads are automatically cleaned up after 24 hours.
- **Range reads** — `GET` requests with a `Range` header are supported and return `206 Partial Content`.
