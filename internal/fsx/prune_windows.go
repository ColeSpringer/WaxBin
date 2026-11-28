package fsx

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"golang.org/x/sys/windows"
)

// device reports nothing on Windows, where a volume mounted on a folder is a reparse
// point that Lstat does not call a folder.
func device(fs.FileInfo) (uint64, bool) { return 0, false }

// volumeNameGUID has GetFinalPathNameByHandle spell a path from its volume's GUID, where
// zero spells it from a drive letter or a share.
const volumeNameGUID = 0x1

// volume reads the volume through a handle, so a junction or a mounted folder answers
// for the volume it reaches. A local volume is named by its GUID, which two disks cloned
// from one image do not share. A share has none and is named by its server and the
// serial number the server reports, the same for every share of one disk there. The
// serial alone names nothing: servers built from one image report the same one.
func volume(path string) (string, bool) {
	name, err := windows.UTF16PtrFromString(pathx.Long(path))
	if err != nil {
		return "", false
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	// \\?\Volume{...}\folder: the name ends at the brace.
	if final, ok := finalName(h, volumeNameGUID); ok {
		if i := strings.IndexByte(final, '}'); i >= 0 {
			return final[:i+1], true
		}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil || info.VolumeSerialNumber == 0 {
		return "", false
	}
	// \\?\UNC\server\share\folder, or \\?\X:\folder for a drive with no GUID.
	final, ok := finalName(h, 0)
	if !ok {
		return "", false
	}
	at := ""
	if rest, share := strings.CutPrefix(final, `\\?\UNC\`); share {
		at, _, _ = strings.Cut(rest, `\`)
		at = `\\` + at
	} else if rest, drive := strings.CutPrefix(final, `\\?\`); drive {
		at, _, _ = strings.Cut(rest, `\`)
	}
	return strings.ToLower(at) + "#" + strconv.FormatUint(uint64(info.VolumeSerialNumber), 16), true
}

// finalName is the path a handle resolves to, its volume spelled as flags ask.
func finalName(h windows.Handle, flags uint32) (string, bool) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", false
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), true
		}
		buf = make([]uint16, n+1)
	}
}

func inUse(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }

// crossDevice reports a rename refused because it would cross volumes, which Windows
// answers with ERROR_NOT_SAME_DEVICE.
func crossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }
