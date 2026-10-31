// Package sigv4test signs test requests with the AWS SDK's SigV4 signer, in the
// Authorization header or as a presigned URL, for every package whose tests
// need a request the server will authenticate.
package sigv4test
