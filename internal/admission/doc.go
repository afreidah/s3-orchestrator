// Package admission divides a fixed number of concurrency slots between
// request flows, queueing the requests that find the pool full and handing
// each freed slot to the request flow holding the fewest. It reports its
// queues through the gauge and observer a caller wires in, and emits no metrics
// of its own.
package admission
