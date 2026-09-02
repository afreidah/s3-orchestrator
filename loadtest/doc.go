// Command loadtest drives constant-rate PUT, GET and mixed workloads against
// any S3-compatible endpoint using Vegeta and SigV4 authentication.
//
// It also runs an object-size sweep, executing the same scenario at several
// sizes and emitting a structured JSON results matrix for performance-envelope
// characterisation, and a ramp mode that raises the rate until an error
// threshold is crossed.
//
// Usage:
//
//	go run . -op put -rate 200 -duration 30s -size 4096
//	go run . -op get -rate 500 -duration 1m -seed 1000
//	go run . -op mixed -rate 300 -duration 2m -seed 500
package main
