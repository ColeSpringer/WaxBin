package waxbin_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

func openBench(b *testing.B, ctx context.Context, db, root string) *waxbin.Library {
	b.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	return lib
}

func writeBench(b *testing.B, path string, data []byte) {
	b.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkScan measures a full catalog scan (walk + tag/essence/content hashing +
// persist) of a synthetic library into a fresh catalog. Scan is I/O-bound and
// never decodes PCM, so this is the indexing throughput signal.
func BenchmarkScan(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	const n = 200
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, fmt.Sprintf("album%d", i%20))
		name := filepath.Join(dir, fmt.Sprintf("track%d.mp3", i))
		// Distinct audio per file (unique seed) so items do not dedup by essence.
		writeBench(b, name, testaudio.BuildMP3WithAudio(
			fmt.Sprintf("Track %d", i), fmt.Sprintf("Artist %d", i%20),
			fmt.Sprintf("Album %d", i%20), i%20+1, testaudio.AudioWithSeed(byte(i))))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db := filepath.Join(b.TempDir(), "c.db")
		lib := openBench(b, ctx, db, root)
		b.StartTimer()

		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		_ = lib.Close()
		b.StartTimer()
	}
}

// BenchmarkAnalyze measures the analyze pass (decode to PCM + fingerprint +
// loudness + peaks) over a set of WAVs. WAV decodes pure-Go, so this benchmark is
// host-independent (no ffmpeg needed). Each iteration analyzes a freshly scanned
// catalog, since analysis is essence-keyed and would otherwise no-op.
func BenchmarkAnalyze(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	const (
		n    = 40
		rate = 22050
	)
	for i := 0; i < n; i++ {
		sig := testaudio.RichSignal(rate, 4, testaudio.MusicalPartials, int64(i+1))
		writeBench(b, filepath.Join(root, fmt.Sprintf("t%d.wav", i)), testaudio.EncodeWAV16(rate, sig))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db := filepath.Join(b.TempDir(), "c.db")
		lib := openBench(b, ctx, db, root)
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()

		if _, err := lib.Analyze(ctx); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		_ = lib.Close()
		b.StartTimer()
	}
}
