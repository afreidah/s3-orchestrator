// Package sharedstate is how the instances of one deployment share what each of
// them knows: a fleet-wide value one instance writes for the others (Value), a
// message every other instance receives (Topic), and each instance's own report
// that any of them can list (Roster). Without Redis there is a single instance,
// and every helper acts locally: a Value is never stored, a Topic reaches no
// one, and a Roster lists this instance alone.
package sharedstate
