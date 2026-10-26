package backup

import "time"

// SettleQuickly shortens the wait between two copies of a SQLite database, for a test.
func SettleQuickly() (restore func()) {
	was := snapshotWait
	snapshotWait = time.Millisecond
	return func() { snapshotWait = was }
}
