package netsafe

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

// FuzzSafeFilename asserts a filename derived from any remote string is safe to
// create cross-platform: non-empty, no path separators or control/reserved
// characters, and never "." or "..".
func FuzzSafeFilename(f *testing.F) {
	for _, s := range []string{
		"http://h/a/b.mp3?token=x", "../../etc/passwd", "CON", `\\srv\share\f.mp3`,
		"name\x00.mp3", "", "https://h", "https://h/", "..", ".", "  ..  ",
		"日本語.mp3", "a?b*c:d.mp3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, remote string) {
		out := SafeFilename(remote, "fallback")
		if out == "" {
			t.Fatalf("SafeFilename(%q) is empty (should fall back)", remote)
		}
		if strings.ContainsAny(out, `/\`) {
			t.Fatalf("SafeFilename(%q) = %q contains a path separator", remote, out)
		}
		if out == "." || out == ".." {
			t.Fatalf("SafeFilename(%q) = %q is a dot name", remote, out)
		}
		for _, r := range out {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("SafeFilename(%q) = %q has a control character", remote, out)
			}
		}
	})
}

// FuzzURLGuards runs the SSRF/URL guards over arbitrary strings to prove they
// never panic on malformed URLs (they gate attacker-supplied feed/enclosure URLs).
func FuzzURLGuards(f *testing.F) {
	for _, s := range []string{
		"http://example.com/path", "ftp://x", "://", "http://[::1]/", "javascript:x",
		"http://user:pass@host:99999/", "https://127.0.0.1", "http://[fe80::1%eth0]/",
		"", "h ttp://x", "http://\x00/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_ = ShownHost(raw)
		_ = checkScheme(raw, "fuzz")
		_ = guardDialAddr(raw)
	})
}

// FuzzBlockedIPEmbeddings asserts every IPv6 form that carries a blocked IPv4 address is
// blocked too, whatever prefix, subnet or Teredo server surrounds it.
func FuzzBlockedIPEmbeddings(f *testing.F) {
	for _, v4 := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.168.1.1", "100.64.0.1", "224.0.0.1",
		"0.0.0.0", "255.255.255.255", "198.18.0.1"} {
		a := netip.MustParseAddr(v4).As4()
		f.Add(a[0], a[1], a[2], a[3], uint64(0x2600_0000_0000_0000))
	}
	f.Fuzz(func(t *testing.T, a, b, c, d byte, around uint64) {
		v4 := netip.AddrFrom4([4]byte{a, b, c, d})
		if !isBlockedIP(v4) {
			return
		}
		var rest [8]byte
		binary.BigEndian.PutUint64(rest[:], around)
		form := func(name string, build func(*[16]byte)) {
			var x [16]byte
			build(&x)
			for _, addr := range []netip.Addr{netip.AddrFrom16(x), netip.AddrFrom16(x).WithZone("eth0")} {
				if !isBlockedIP(addr) {
					t.Fatalf("%s %s of blocked %s is not blocked", name, addr, v4)
				}
			}
		}
		v := v4.As4()
		form("mapped", func(x *[16]byte) { x[10], x[11] = 0xff, 0xff; copy(x[12:], v[:]) })
		form("NAT64", func(x *[16]byte) { copy(x[:4], []byte{0, 0x64, 0xff, 0x9b}); copy(x[12:], v[:]) })
		form("6to4", func(x *[16]byte) { x[0], x[1] = 0x20, 0x02; copy(x[2:6], v[:]); copy(x[6:], rest[:]) })
		form("Teredo server", func(x *[16]byte) { x[0], x[1] = 0x20, 0x01; copy(x[4:8], v[:]); copy(x[8:], rest[:]) })
		form("Teredo client", func(x *[16]byte) {
			x[0], x[1] = 0x20, 0x01
			copy(x[4:12], rest[:])
			for i := range 4 {
				x[12+i] = ^v[i]
			}
		})
		for _, iid := range [][]byte{{0, 0, 0x5e, 0xfe}, {2, 0, 0x5e, 0xfe}} {
			form("ISATAP", func(x *[16]byte) { copy(x[:8], rest[:]); copy(x[8:12], iid); copy(x[12:], v[:]) })
		}
		form("compatible", func(x *[16]byte) { copy(x[12:], v[:]) })
		form("translated", func(x *[16]byte) { x[8], x[9] = 0xff, 0xff; copy(x[12:], v[:]) })
	})
}
