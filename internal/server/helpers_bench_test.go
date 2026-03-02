package server

import "testing"

func BenchmarkParsePath(b *testing.B) {
	paths := []struct {
		name, path string
	}{
		{"bucket_only", "/mybucket"},
		{"bucket_and_key", "/mybucket/photos/image.jpg"},
		{"deep_path", "/mybucket/a/b/c/d/e/f/g/file.txt"},
	}

	for _, p := range paths {
		b.Run(p.name, func(b *testing.B) {
			for b.Loop() {
				parsePath(p.path)
			}
		})
	}
}
