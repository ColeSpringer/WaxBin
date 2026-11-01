package port

// SwapSyncPath replaces the sync WriteBackup runs on its part file and folder, for a test,
// returning what puts it back.
func SwapSyncPath(f func(string) error) (restore func()) {
	old := syncPath
	syncPath = f
	return func() { syncPath = old }
}
