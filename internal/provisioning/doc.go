// Package provisioning merges the two sources a deployment declares buckets and
// credentials in - the config file and the store - into one view of what exists.
//
// A deployment can declare a bucket in either place, and a credential belongs to
// a user held in the store or is implied by a bucket the config file declares.
// Both have to resolve identically at request time and appear side by side in an
// operator's listing, which is what this package produces: one set of buckets,
// users, credentials and grants, each carrying where it came from.
//
// It exists as its own package because two consumers need the same union and
// neither can own it. The auth package builds the request-time registry from it
// and must not read a store; the operations layer serves it to the provisioning
// API and must not reach into transport. Both depend on this, and it depends on
// neither.
package provisioning
