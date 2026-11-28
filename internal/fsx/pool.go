package fsx

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Pool names the free space path draws on, so that two paths with one answer fill each
// other's room. It is Volume's answer except where filesystems share a pool under
// devices of their own (btrfs subvolumes, ZFS datasets, the volumes of an APFS
// container), which answer alike; Volume keeps naming the device. Where the probe for a
// pool fails, Pool answers Volume. ok is false when path is not there or the platform
// cannot say.
func Pool(path string) (id string, ok bool) { return pool(path) }

// MountPoints lists the folders filesystems are mounted on, spelled as the system's mount
// table spells them (every link resolved). ok is false where the table cannot be read or
// the platform has none.
func MountPoints() (points []string, ok bool) { return mountPoints() }

// PoolPeers names folders that draw on the same pool as path, mounted where they can be
// read: the other datasets of a ZFS pool, its top dataset first, and the other volumes
// of an APFS container, at most maxPeers of them. A caller weighing what several members
// of a pool report, each perhaps under a quota of its own, reads these as well, since one
// under no quota reports the pool's room. It names none where the filesystem shares no
// pool or the probe fails.
func PoolPeers(path string) []string { return peers(path) }

// maxPeers bounds PoolPeers: a pool holding a dataset per container image mounts
// thousands, and one under no quota is all a reading needs.
const maxPeers = 16

// peer is a candidate for PoolPeers: where it is mounted, and whether it is the pool's
// top dataset, which is read first.
type peer struct {
	point string
	top   bool
}

// peerOf reports whether a mount from source can stand for its pool's room, and whether
// it is the pool's top dataset: a ZFS dataset (not a snapshot, which takes nothing), or
// any APFS volume.
func peerOf(fstype, source string) (ok, top bool) {
	switch fstype {
	case "zfs":
		return source != "" && !strings.Contains(source, "@"), !strings.Contains(source, "/")
	case "apfs":
		return true, false
	}
	return false, false
}

// pickPeers orders the candidates top first, drops repeats, and keeps maxPeers.
func pickPeers(cands []peer) []string {
	slices.SortStableFunc(cands, func(a, b peer) int {
		switch {
		case a.top == b.top:
			return 0
		case a.top:
			return -1
		}
		return 1
	})
	var out []string
	for _, c := range cands {
		if len(out) == maxPeers {
			break
		}
		if !slices.Contains(out, c.point) {
			out = append(out, c.point)
		}
	}
	return out
}

// poolOfSource names the pool a mount's source draws on, for the filesystems whose
// source says it: a ZFS dataset names its pool before the first / or @, and an APFS
// volume's device names its container (/dev/disk3s1s1 is on disk3, and a snapshot is
// mounted from name@device).
func poolOfSource(fstype, source string) (string, bool) {
	switch fstype {
	case "zfs":
		name := source
		if i := strings.IndexAny(name, "/@"); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			return "", false
		}
		return "zfs:" + name, true
	case "apfs":
		if i := strings.LastIndexByte(source, '@'); i >= 0 {
			source = source[i+1:]
		}
		dev, ok := strings.CutPrefix(strings.TrimPrefix(source, "/dev/"), "disk")
		container, tail := digits(dev)
		if !ok || container == "" {
			return "", false
		}
		for tail != "" {
			slice, ok := strings.CutPrefix(tail, "s")
			n, more := digits(slice)
			if !ok || n == "" {
				return "", false
			}
			tail = more
		}
		return "apfs:disk" + container, true
	}
	return "", false
}

// digits splits s after its leading decimal digits.
func digits(s string) (lead, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], s[i:]
}

// mountLine is one line of /proc/self/mountinfo: the mount's id, the device of its
// superblock as major:minor, where it is mounted, its filesystem type and its source.
type mountLine struct {
	id, dev, point, fstype, source string
}

// parseMounts reads mountinfo's lines, skipping any that lack the "-" that ends the
// optional fields after the seventh.
func parseMounts(mountinfo []byte) []mountLine {
	var out []mountLine
	for line := range bytes.Lines(mountinfo) {
		fields := strings.Fields(string(line))
		for i := 6; i+2 < len(fields); i++ {
			if fields[i] == "-" {
				out = append(out, mountLine{id: fields[0], dev: fields[2], point: unescapeMount(fields[4]), fstype: fields[i+1], source: fields[i+2]})
				break
			}
		}
	}
	return out
}

// mountPool names the pool of the mount numbered id. A source that names its pool
// (poolOfSource) gives it; any other mount draws on the device of its superblock, which
// every subvolume mount of one btrfs filesystem shares, in the form a stat reports
// st_dev, so that it matches Volume where nothing is shared.
func mountPool(mountinfo []byte, id uint64) (string, bool) {
	want := strconv.FormatUint(id, 10)
	for _, m := range parseMounts(mountinfo) {
		if m.id != want {
			continue
		}
		if p, ok := poolOfSource(m.fstype, m.source); ok {
			return p, true
		}
		maj, mnr, found := strings.Cut(m.dev, ":")
		major, err1 := strconv.ParseUint(maj, 10, 32)
		minor, err2 := strconv.ParseUint(mnr, 10, 32)
		if !found || err1 != nil || err2 != nil {
			return "", false
		}
		dev := minor&0xff | (major&0xfff)<<8 | (minor&^0xff)<<12 | (major&^0xfff)<<32
		return strconv.FormatUint(dev, 10), true
	}
	return "", false
}

// mountPeers reads PoolPeers for the mount numbered id: the mount points of the peers of
// its pool, in the table's order.
func mountPeers(mountinfo []byte, id uint64) []string {
	mounts := parseMounts(mountinfo)
	want := strconv.FormatUint(id, 10)
	own := slices.IndexFunc(mounts, func(m mountLine) bool { return m.id == want })
	if own < 0 {
		return nil
	}
	pool, ok := poolOfSource(mounts[own].fstype, mounts[own].source)
	if !ok {
		return nil
	}
	var cands []peer
	for _, m := range mounts {
		if p, ok := poolOfSource(m.fstype, m.source); ok && p == pool {
			if ok, top := peerOf(m.fstype, m.source); ok {
				cands = append(cands, peer{point: m.point, top: top})
			}
		}
	}
	return pickPeers(cands)
}

// mountPointsOf lists the mount points of mountinfo, in the table's order.
func mountPointsOf(mountinfo []byte) []string {
	var out []string
	for _, m := range parseMounts(mountinfo) {
		out = append(out, m.point)
	}
	return out
}

// unescapeMount undoes the octal escapes mountinfo writes for a space, a tab, a newline
// and a backslash in a mount point.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// PoolRoom asks the pool path draws on for its own room, where its filesystem has a tool
// that reports it: zfs get for a ZFS pool's available bytes, diskutil apfs list for an
// APFS container's free bytes on macOS. That is the room a member under no quota reports,
// read even where none is mounted. It runs a command, bounded by RootCheckTimeout, so a
// caller asks it only when what the members report cannot settle the pool's room. ok is
// false where the filesystem shares no pool or no tool answers.
func PoolRoom(path string) (int64, bool) {
	id, ok := Pool(path)
	if !ok {
		return 0, false
	}
	if name, ok := strings.CutPrefix(id, "zfs:"); ok && !strings.HasPrefix(name, "-") {
		out, ok := runTool("zfs", []string{"/sbin/zfs", "/usr/sbin/zfs", "/usr/local/sbin/zfs"}, "get", "-Hp", "-o", "value", "available", name)
		if !ok {
			return 0, false
		}
		return parseZFSAvailable(out)
	}
	if container, ok := strings.CutPrefix(id, "apfs:"); ok && runtime.GOOS == "darwin" {
		out, ok := runTool("diskutil", []string{"/usr/sbin/diskutil"}, "apfs", "list", "-plist")
		if !ok {
			return 0, false
		}
		return apfsContainerFree(out, container)
	}
	return 0, false
}

// runTool runs a system tool found on the path or at the first of where that is there,
// and returns what it wrote to its standard output.
func runTool(name string, where []string, args ...string) ([]byte, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		path = ""
		for _, p := range where {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path == "" {
			return nil, false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), RootCheckTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	return out, err == nil
}

// parseZFSAvailable reads zfs get -Hp -o value available: one count of bytes.
func parseZFSAvailable(out []byte) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n, err == nil && n >= 0
}

// apfsContainerFree reads diskutil apfs list -plist for the free bytes of one container.
func apfsContainerFree(plist []byte, container string) (int64, bool) {
	root, err := plistValue(xml.NewDecoder(bytes.NewReader(plist)))
	if err != nil {
		return 0, false
	}
	top, _ := root.(map[string]any)
	containers, _ := top["Containers"].([]any)
	for _, c := range containers {
		if m, ok := c.(map[string]any); ok && m["ContainerReference"] == container {
			n, ok := m["CapacityFree"].(int64)
			return n, ok
		}
	}
	return 0, false
}

// plistEnd stands for the end of the dict or array being read.
type plistEnd struct{}

type plistKey string

// plistValue reads the next value of a property list: a dict as a map, an array as a
// slice, a string, an integer as an int64, true and false; any other element reads as nil.
func plistValue(d *xml.Decoder) (any, error) {
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return plistEnd{}, nil
		case xml.StartElement:
			switch t.Name.Local {
			case "plist":
				continue
			case "dict":
				m := map[string]any{}
				for {
					k, err := plistValue(d)
					if err != nil {
						return nil, err
					}
					if _, end := k.(plistEnd); end {
						return m, nil
					}
					key, ok := k.(plistKey)
					if !ok {
						return nil, errors.New("plist: a dict entry with no key")
					}
					v, err := plistValue(d)
					if err != nil {
						return nil, err
					}
					m[string(key)] = v
				}
			case "array":
				var a []any
				for {
					v, err := plistValue(d)
					if err != nil {
						return nil, err
					}
					if _, end := v.(plistEnd); end {
						return a, nil
					}
					a = append(a, v)
				}
			case "key", "string", "integer":
				var text string
				if err := d.DecodeElement(&text, &t); err != nil {
					return nil, err
				}
				switch t.Name.Local {
				case "key":
					return plistKey(text), nil
				case "integer":
					return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
				}
				return text, nil
			case "true", "false":
				return t.Name.Local == "true", d.Skip()
			default:
				return nil, d.Skip()
			}
		}
	}
}
