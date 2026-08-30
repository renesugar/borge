// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// What a fresh zstd encoder per chunk costs, which §12.2 measured at 4.7x and then left
// unfixed for a week because the default compression is lz4.
//
//	go test ./internal/compress/ -run '^$' -bench 'Zstd(Fresh|Pooled)' -benchmem

func zstdBenchData() []byte {
	b := make([]byte, 2<<20) // §12.2's 2 MiB buffer, so the numbers are comparable
	rng := rand.New(rand.NewSource(5))
	// Semi-compressible: random blocks repeated, so the encoder has real work to do.
	block := make([]byte, 4096)
	rng.Read(block)
	for i := 0; i < len(b); i += len(block) {
		copy(b[i:], block)
	}
	return b
}

// BenchmarkZstdFresh is what borge did: an encoder per call.
func BenchmarkZstdFresh(b *testing.B) {
	data := zstdBenchData()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			b.Fatal(err)
		}
		_ = enc.EncodeAll(data, nil)
		enc.Close()
	}
}

// BenchmarkZstdPooled is what it does now.
func BenchmarkZstdPooled(b *testing.B) {
	data := zstdBenchData()
	c := Zstd{level: 3}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		var meta Meta
		if _, err := c.Compress(&meta, data); err != nil {
			b.Fatal(err)
		}
	}
}
