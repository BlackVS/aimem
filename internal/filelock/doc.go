// Package filelock takes an exclusive, non-blocking lock on an open file:
// flock on Unix, LockFileEx on Windows. The lock is advisory between
// processes that take it, and the OS releases it when its holder exits, so
// a crashed holder never leaves it stale.
package filelock
