// Package object is the object read and write path: placement, replication
// fan-out, failover across the copies of a key, and the stored-form transforms
// (encryption, compression) a body passes through in each direction. The
// transport layers call into it; it calls the backends and the ledger.
package object
