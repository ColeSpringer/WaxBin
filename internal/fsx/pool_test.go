package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestPoolOfSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fstype, source string
		want           string
		ok             bool
	}{
		{"zfs", "tank", "zfs:tank", true},
		{"zfs", "tank/music", "zfs:tank", true},
		{"zfs", "tank/music/live@2026-11-27", "zfs:tank", true},
		{"zfs", "tank@daily", "zfs:tank", true},
		{"zfs", "/tank", "", false},
		{"zfs", "", "", false},
		{"apfs", "/dev/disk3s1s1", "apfs:disk3", true},
		{"apfs", "/dev/disk3s5", "apfs:disk3", true},
		{"apfs", "/dev/disk12s2", "apfs:disk12", true},
		{"apfs", "com.apple.TimeMachine.2026-11-27-101500.local@/dev/disk3s5", "apfs:disk3", true},
		{"apfs", "/dev/disk3", "apfs:disk3", true},
		{"apfs", "/dev/disk", "", false},
		{"apfs", "/dev/disk3x1", "", false},
		{"apfs", "/dev/disk3s", "", false},
		{"apfs", "/dev/sdb2", "", false},
		{"ext4", "/dev/sda1", "", false},
		{"btrfs", "/dev/loop0", "", false},
		{"hfs", "/dev/disk2s2", "", false},
	}
	for _, tc := range tests {
		got, ok := poolOfSource(tc.fstype, tc.source)
		if got != tc.want || ok != tc.ok {
			t.Errorf("poolOfSource(%q, %q) = %q, %v; want %q, %v", tc.fstype, tc.source, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMountPool(t *testing.T) {
	t.Parallel()
	const mountinfo = `22 1 8:64 / / rw,relatime - ext4 /dev/sde rw,errors=remount-ro
30 22 0:45 / /mnt/top rw,relatime shared:1 - btrfs /dev/loop0 rw,space_cache=v2,subvolid=5,subvol=/
31 22 0:45 /b /mnt/b rw,relatime shared:2 master:1 - btrfs /dev/loop0 rw,space_cache=v2,subvolid=257,subvol=/b
40 22 0:52 / /tank/a rw,relatime - zfs waxbina/a rw,xattr,posixacl
41 22 0:53 / /tank/b rw,relatime - zfs waxbina/b rw,xattr,posixacl
42 22 0:54 / /mnt/My\040Music rw,relatime - ext4 /dev/sdb1 rw
43 22 259:3 / /data rw,relatime - xfs /dev/nvme0n1p3 rw
44 22 0:300 / /run/user/1000 rw,relatime - tmpfs tmpfs rw
45 22 0:60 / /broken rw,relatime
`
	// The device numbers are the st_dev a stat reports for the same major:minor.
	tests := []struct {
		id   uint64
		want string
		ok   bool
	}{
		{22, "2112", true},
		{30, "45", true},
		{31, "45", true},
		{40, "zfs:waxbina", true},
		{41, "zfs:waxbina", true},
		{42, "54", true},
		{43, "66307", true},
		{44, "1048620", true},
		{45, "", false},
		{99, "", false},
	}
	for _, tc := range tests {
		got, ok := mountPool([]byte(mountinfo), tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("mountPool(%d) = %q, %v; want %q, %v", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPool: two folders of one filesystem draw on one pool, a link draws on the pool of
// the folder it reaches, and a path that is not there draws on none.
func TestPool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pa, ok := Pool(a)
	if !ok || pa == "" {
		t.Fatalf("Pool(%q) = %q, %v, want a pool", a, pa, ok)
	}
	if pb, ok := Pool(b); !ok || pb != pa {
		t.Errorf("Pool(%q) = %q, %v, want %q as for the folder beside it", b, pb, ok, pa)
	}
	if p, ok := Pool(filepath.Join(dir, "gone")); ok {
		t.Errorf("a missing path draws on pool %q", p)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(b, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if pl, ok := Pool(link); !ok || pl != pa {
		t.Errorf("Pool of a link = %q, %v, want %q as for the folder it reaches", pl, ok, pa)
	}
}

func TestMountPeers(t *testing.T) {
	t.Parallel()
	const mountinfo = `40 22 0:52 / /tank/a rw,relatime - zfs waxbina/a rw
41 22 0:53 / /tank/b rw,relatime - zfs waxbina/b rw
48 41 0:58 / /tank/b/.zfs/snapshot/daily rw,relatime - zfs waxbina/b@daily ro
42 22 0:51 / /tank rw,relatime - zfs waxbina rw
43 22 0:55 / /other rw,relatime - zfs waxbinb rw
44 22 0:56 / /mnt/My\040Pool rw,relatime - zfs pool2 rw
45 22 0:57 / /mnt/x rw,relatime - zfs pool2/x rw
46 22 8:64 / / rw,relatime - ext4 /dev/sde rw
47 22 0:45 / /mnt/top rw,relatime - btrfs /dev/loop0 rw
`
	tests := []struct {
		id   uint64
		want []string
	}{
		{40, []string{"/tank", "/tank/a", "/tank/b"}},
		{42, []string{"/tank", "/tank/a", "/tank/b"}},
		{45, []string{"/mnt/My Pool", "/mnt/x"}},
		{46, nil},
		{47, nil},
		{99, nil},
	}
	for _, tc := range tests {
		if got := mountPeers([]byte(mountinfo), tc.id); !slices.Equal(got, tc.want) {
			t.Errorf("mountPeers(%d) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestMountPointsOf(t *testing.T) {
	t.Parallel()
	const mountinfo = `22 1 8:64 / / rw,relatime - ext4 /dev/sde rw
40 22 0:52 / /tank/a rw,relatime - zfs waxbina/a rw
44 22 0:56 / /mnt/My\040Music rw,relatime - ext4 /dev/sdb1 rw
45 22 0:60 / /broken rw,relatime
`
	want := []string{"/", "/tank/a", "/mnt/My Music"}
	if got := mountPointsOf([]byte(mountinfo)); !slices.Equal(got, want) {
		t.Errorf("mountPointsOf = %q, want %q", got, want)
	}
}

// TestMountPointsListsTheRootFolder: the system's own table is read where there is one.
func TestMountPointsListsTheRootFolder(t *testing.T) {
	t.Parallel()
	points, ok := MountPoints()
	if !ok {
		t.Skip("no mount table to read here")
	}
	if !slices.Contains(points, "/") {
		t.Errorf("MountPoints() = %q, want / among them", points)
	}
}

// TestPickPeers: the top dataset comes first, repeats go, and at most maxPeers are read.
func TestPickPeers(t *testing.T) {
	t.Parallel()
	var cands []peer
	for i := range maxPeers + 4 {
		cands = append(cands, peer{point: "/tank/d" + strconv.Itoa(i)})
	}
	cands = append(cands, peer{point: "/tank/d0"}, peer{point: "/tank", top: true})
	got := pickPeers(cands)
	if len(got) != maxPeers || got[0] != "/tank" || got[1] != "/tank/d0" || slices.Index(got[2:], "/tank/d0") >= 0 {
		t.Errorf("pickPeers = %q, want /tank first, then the first %d datasets once each", got, maxPeers-1)
	}
}

func TestParseZFSAvailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		out  string
		want int64
		ok   bool
	}{
		{"912680550400\n", 912680550400, true},
		{"  12345  \n", 12345, true},
		{"-\n", 0, false},
		{"", 0, false},
		{"12 34\n", 0, false},
	}
	for _, tc := range tests {
		if got, ok := parseZFSAvailable([]byte(tc.out)); got != tc.want || ok != tc.ok {
			t.Errorf("parseZFSAvailable(%q) = %d, %v; want %d, %v", tc.out, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAPFSContainerFree(t *testing.T) {
	t.Parallel()
	const plist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Containers</key>
	<array>
		<dict>
			<key>APFSContainerUUID</key>
			<string>A2B4C6D8-0000-4000-8000-000000000001</string>
			<key>CapacityCeiling</key>
			<integer>494384795648</integer>
			<key>CapacityFree</key>
			<integer>182000000000</integer>
			<key>ContainerReference</key>
			<string>disk3</string>
			<key>Fusion</key>
			<false/>
			<key>Volumes</key>
			<array>
				<dict>
					<key>CapacityQuota</key>
					<integer>0</integer>
					<key>DeviceIdentifier</key>
					<string>disk3s1</string>
				</dict>
			</array>
		</dict>
		<dict>
			<key>CapacityFree</key>
			<integer>201326592</integer>
			<key>ContainerReference</key>
			<string>disk5</string>
		</dict>
	</array>
</dict>
</plist>
`
	for _, tc := range []struct {
		container string
		want      int64
		ok        bool
	}{
		{"disk3", 182000000000, true},
		{"disk5", 201326592, true},
		{"disk7", 0, false},
	} {
		if got, ok := apfsContainerFree([]byte(plist), tc.container); got != tc.want || ok != tc.ok {
			t.Errorf("apfsContainerFree(%s) = %d, %v; want %d, %v", tc.container, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := apfsContainerFree([]byte("<plist><dict><key>Containers</key>"), "disk3"); ok {
		t.Error("a cut-off plist gave a room")
	}
}
