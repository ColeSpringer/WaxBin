//go:build !windows

package pathx

// Long is a no-op off Windows: POSIX filesystems impose no MAX_PATH limit, so the
// absolute path is used as-is. It lets callers wrap every filesystem operation in
// Long unconditionally, with the prefixing logic confined to the Windows build.
func Long(abs string) string { return abs }
