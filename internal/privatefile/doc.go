// Package privatefile creates and checks files that hold one secret: an
// operator's admin token, an issued peer bearer, or the hub's outbound
// introspection credential. A secret file must be readable by its owner
// only; Check measures that by mode bits on Unix and by the effective access
// control list on Windows.
package privatefile
