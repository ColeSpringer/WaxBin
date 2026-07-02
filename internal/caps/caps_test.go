package caps

import "testing"

func TestParseImageDecoders(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		avif, heic bool
	}{
		{
			// Native names (FFmpeg >= 5.1).
			name: "native",
			out: " V..... av1                  Alliance for Open Media AV1\n" +
				" V..... hevc                 H.265 / HEVC\n",
			avif: true, heic: true,
		},
		{
			// External libs are the common real-world shape and MUST be recognized; an
			// exact-name match would miss them, silently disabling exotic thumbnails.
			name: "external libs",
			out: " V..... libdav1d             dav1d AV1 decoder\n" +
				" V..... libde265             libde265 HEVC decoder\n",
			avif: true, heic: true,
		},
		{
			name: "libaom av1 only",
			out:  " V..... libaom-av1           libaom AV1 decoder\n",
			avif: true, heic: false,
		},
		{
			// A decoder list without either codec disables both.
			name: "neither",
			out: " V..... mjpeg                Motion JPEG\n" +
				" V..... png                  PNG image\n",
			avif: false, heic: false,
		},
		{
			name: "empty",
			out:  "",
			avif: false, heic: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			avif, heic := parseImageDecoders([]byte(tc.out))
			if avif != tc.avif || heic != tc.heic {
				t.Errorf("parseImageDecoders = (avif=%v, heic=%v), want (avif=%v, heic=%v)", avif, heic, tc.avif, tc.heic)
			}
		})
	}
}
