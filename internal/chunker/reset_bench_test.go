// SPDX-License-Identifier: Apache-2.0

package chunker

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// What building a chunker per file costs, and what reusing one saves.
//
// §12.1 measured construction alone. These measure the thing that actually matters: the
// cost of chunking a *small* file, where construction is not a rounding error but most of
// the work. The corpus the project brief names is 118,866 files in one directory, and the
// median file in it is a few kilobytes.
//
// Run with:
//
//	go test ./internal/chunker/ -run '^$' -bench 'PerFile|Reused' -benchmem

// benchFile is a small file: big enough to be a real chunking call, small enough that
// table construction dominates - which is the case the fix is for.
const benchFileSize = 4 << 10

func benchData(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(7)).Read(b)
	return b
}

func drain(b *testing.B, c Chunker) {
	for {
		if _, err := c.Next(); err == io.EOF {
			return
		} else if err != nil {
			b.Fatal(err)
		}
	}
}

// benchmarkPerFile is what borge did: a chunker per file.
func benchmarkPerFile(b *testing.B, p Params) {
	key := make([]byte, 32)
	data := benchData(benchFileSize)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		c, err := New(p, key, 0, bytes.NewReader(data))
		if err != nil {
			b.Fatal(err)
		}
		drain(b, c)
	}
}

// benchmarkReused is what borg does, and what borge does now.
func benchmarkReused(b *testing.B, p Params) {
	key := make([]byte, 32)
	data := benchData(benchFileSize)
	c, err := New(p, key, 0, bytes.NewReader(nil))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		c.Reset(bytes.NewReader(data))
		drain(b, c)
	}
}

func BenchmarkFastCDCPerFile(b *testing.B) { benchmarkPerFile(b, DefaultParams()) }
func BenchmarkFastCDCReused(b *testing.B)  { benchmarkReused(b, DefaultParams()) }

func buzhash64Params() Params {
	return Params{Algorithm: AlgoBuzhash64, ChunkMinExp: 19, ChunkMaxExp: 23,
		HashMaskBits: 21, WindowSize: 4095, NCLevel: 2}
}

func BenchmarkBuzhash64PerFile(b *testing.B) { benchmarkPerFile(b, buzhash64Params()) }
func BenchmarkBuzhash64Reused(b *testing.B)  { benchmarkReused(b, buzhash64Params()) }

// Where the per-file cost actually was.
//
// §12.1 attributed it to the keyed tables, measuring 1.75 ms of setup for fastcdc. The
// tables are real but they are the smaller half: driver.init allocates a backing buffer of
// exactly maxSize, which at the default ChunkMaxExp of 23 is 8 MiB, and Go zeroes it. That
// is the 8.4 MB/op the per-file benchmark reports.
//
// These two split it, so the plan can say which is which rather than repeat the guess.

// BenchmarkNewFastCDC is construction at borg's default parameters: tables plus an 8 MiB
// buffer.
func BenchmarkNewFastCDC(b *testing.B) {
	key := make([]byte, 32)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(DefaultParams(), key, 0, bytes.NewReader(nil)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNewFastCDCSmallBuffer is the same construction with a 64 KiB maximum chunk, so
// what remains is the table derivation. The difference between the two is the buffer.
func BenchmarkNewFastCDCSmallBuffer(b *testing.B) {
	key := make([]byte, 32)
	p := DefaultParams()
	p.ChunkMinExp, p.ChunkMaxExp, p.HashMaskBits = 12, 16, 14
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(p, key, 0, bytes.NewReader(nil)); err != nil {
			b.Fatal(err)
		}
	}
}
