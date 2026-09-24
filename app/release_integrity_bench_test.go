package app

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkReleaseIntegrity measures warm-cache verification of synthetic releases.
// Run with: go test ./app -run '^$' -bench '^BenchmarkReleaseIntegrity$' -benchtime=3x -count=3
// Files and manifests are generated outside the timed section in b.TempDir.
// Results depend on filesystem, page cache, and machine; this is not a cold-cache test.
func BenchmarkReleaseIntegrity(b *testing.B) {
	for _, shape := range []struct {
		name     string
		files    int
		fileSize int
	}{
		{"small/many", 1_000, 4 << 10},
		{"small/large", 4, 1 << 20},
		{"medium/many", 10_000, 4 << 10},
		{"medium/large", 10, 4 << 20},
		{"large/many", 30_000, 4 << 10},
		{"large/large", 12, 10 << 20},
	} {
		b.Run(shape.name, func(b *testing.B) {
			b.StopTimer()
			release := b.TempDir()
			payload := filepath.Join(release, venvDir, "lib", "python3", "site-packages", "payload")
			data := make([]byte, shape.fileSize)
			for i := range data {
				data[i] = byte(i*31 + 17)
			}
			for i := 0; i < shape.files; i++ {
				dir := filepath.Join(payload, fmt.Sprintf("package%04d", i/100))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("module%06d.py", i)), data, 0o644); err != nil {
					b.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(release, "tool.py"), []byte("# archived source\n"), 0o644); err != nil {
				b.Fatal(err)
			}
			cfg := config{raw: map[string]any{"name": "bench", "source": "tool.py"}}
			if err := writeReleaseMetadata(release, release, cfg, "benchmark"); err != nil {
				b.Fatal(err)
			}
			if err := verifyReleaseIntegrity(release, cfg); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(shape.files * shape.fileSize))
			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if err := verifyReleaseIntegrity(release, cfg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
