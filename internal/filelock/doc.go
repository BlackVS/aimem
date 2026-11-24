// Package filelock takes an exclusive (TryLock) or shared (TryLockShared),
// non-blocking lock on an open file: flock on Unix, LockFileEx on Windows.
// Any number of handles hold it shared at once, and none while one holds
// it exclusive. The lock is advisory between processes that take it, and
// the OS releases it when its holder exits, so a crashed holder never
// leaves it stale. Unlock releases either kind.
package filelock
