// Package httpserver constructs the HTTP listener the daemon serves S3,
// admin, UI, health, and metrics traffic on. It owns route registration,
// middleware composition, TLS config (including the cert reloader for
// SIGHUP rotation), and the optional separate metrics listener.
package httpserver
