// Copyright 2011 The Snappy-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package snappy

import (
	"encoding/binary"
	"errors"
	"io"
)

func load32(b []byte, i int) uint32 {
	b = b[i : i+4 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load64(b []byte, i int) uint64 {
	b = b[i : i+8 : len(b)] // Help the compiler eliminate bounds checks on the next line.
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// emitLiteral writes a literal chunk and returns the number of bytes written.
//
// It assumes that:
//	dst is long enough to hold the encoded bytes
//	1 <= len(lit) && len(lit) <= 65536
func emitLiteral(dst, lit []byte) int {
	i, n := 0, uint(len(lit)-1)
	switch {
	case n < 60:
		dst[0] = uint8(n)<<2 | tagLiteral
		i = 1
	case n < 1<<8:
		dst[0] = 60<<2 | tagLiteral
		dst[1] = uint8(n)
		i = 2
	default:
		dst[0] = 61<<2 | tagLiteral
		dst[1] = uint8(n)
		dst[2] = uint8(n >> 8)
		i = 3
	}
	return i + copy(dst[i:], lit)
}

// emitCopy writes a copy chunk and returns the number of bytes written.
//
// It assumes that:
//	dst is long enough to hold the encoded bytes
//	1 <= offset && offset <= 65535
//	4 <= length && length <= 65535
func emitCopy(dst []byte, offset, length int) int {
	i := 0
	// The maximum length for a single tagCopy1 or tagCopy2 op is 64 bytes. The
	// threshold for this loop is a little higher (at 68 = 64 + 4), and the
	// length emitted down below is is a little lower (at 60 = 64 - 4), because
	// it's shorter to encode a length 67 copy as a length 60 tagCopy2 followed
	// by a length 7 tagCopy1 (which encodes as 3+2 bytes) than to encode it as
	// a length 64 tagCopy2 followed by a length 3 tagCopy2 (which encodes as
	// 3+3 bytes). The magic 4 in the 64±4 is because the minimum length for a
	// tagCopy1 op is 4 bytes, which is why a length 3 copy has to be an
	// encodes-as-3-bytes tagCopy2 instead of an encodes-as-2-bytes tagCopy1.
	for length >= 68 {
		// Emit a length 64 copy, encoded as 3 bytes.
		dst[i+0] = 63<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		i += 3
		length -= 64
	}
	if length > 64 {
		// Emit a length 60 copy, encoded as 3 bytes.
		dst[i+0] = 59<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		i += 3
		length -= 60
	}
	if length >= 12 || offset >= 2048 {
		// Emit the remaining copy, encoded as 3 bytes.
		dst[i+0] = uint8(length-1)<<2 | tagCopy2
		dst[i+1] = uint8(offset)
		dst[i+2] = uint8(offset >> 8)
		return i + 3
	}
	// Emit the remaining copy, encoded as 2 bytes.
	dst[i+0] = uint8(offset>>8)<<5 | uint8(length-4)<<2 | tagCopy1
	dst[i+1] = uint8(offset)
	return i + 2
}

// Encode returns the encoded form of src. The returned slice may be a sub-
// slice of dst if dst was large enough to hold the entire encoded block.
// Otherwise, a newly allocated slice will be returned.
//
// It is valid to pass a nil dst.
func Encode(dst, src []byte) []byte {
	if n := MaxEncodedLen(len(src)); n < 0 {
		panic(ErrTooLarge)
	} else if len(dst) < n {
		dst = make([]byte, n)
	}

	// The block starts with the varint-encoded length of the decompressed bytes.
	d := binary.PutUvarint(dst, uint64(len(src)))

	for len(src) > 0 {
		p := src
		src = nil
		if len(p) > maxBlockSize {
			p, src = p[:maxBlockSize], p[maxBlockSize:]
		}
		if len(p) < minNonLiteralBlockSize {
			d += emitLiteral(dst[d:], p)
		} else {
			d += encodeBlock(dst[d:], p)
		}
	}
	return dst[:d]
}

// inputMargin is the minimum number of extra input bytes to keep, inside
// encodeBlock's inner loop. On some architectures, this margin lets us
// implement a fast path for emitLiteral, where the copy of short (<= 16 byte)
// literals can be implemented as a single load to and store from a 16-byte
// register. That literal's actual length can be as short as 1 byte, so this
// can copy up to 15 bytes too much, but that's OK as subsequent iterations of
// the encoding loop will fix up the copy overrun, and this inputMargin ensures
// that we don't overrun the dst and src buffers.
//
// TODO: implement this fast path.
const inputMargin = 16 - 1

// minNonLiteralBlockSize is the minimum size of the input to encodeBlock that
// could be encoded with a copy tag. This is the minimum with respect to the
// algorithm used by encodeBlock, not a minimum enforced by the file format.
//
// The encoded output must start with at least a 1 byte literal, as there are
// no previous bytes to copy. A minimal (1 byte) copy after that, generated
// from an emitCopy call in encodeBlock's main loop, would require at least
// another inputMargin bytes, for the reason above: we want any emitLiteral
// calls inside encodeBlock's main loop to use the fast path if possible, which
// requires being able to overrun by inputMargin bytes. Thus,
// minNonLiteralBlockSize equals 1 + 1 + inputMargin.
//
// The C++ code doesn't use this exact threshold, but it could, as discussed at
// https://groups.google.com/d/topic/snappy-compression/oGbhsdIJSJ8/discussion
// The difference between Go (2+inputMargin) and C++ (inputMargin) is purely an
// optimization. It should not affect the encoded form. This is tested by
// TestSameEncodingAsCppShortCopies.
const minNonLiteralBlockSize = 1 + 1 + inputMargin

func hash(u, shift uint32) uint32 {
	return (u * 0x1e35a7bd) >> shift
}

// encodeBlock encodes a non-empty src to a guaranteed-large-enough dst. It
// assumes that the varint-encoded length of the decompressed bytes has already
// been written.
//
// It also assumes that:
//	len(dst) >= MaxEncodedLen(len(src)) &&
// 	minNonLiteralBlockSize <= len(src) && len(src) <= maxBlockSize
func encodeBlock(dst, src []byte) (d int) {
	// Initialize the hash table. Its size ranges from 1<<8 to 1<<14 inclusive.
	// The table element type is uint16, as s < sLimit and sLimit < len(src)
	// and len(src) <= maxBlockSize and maxBlockSize == 65536.
	const (
		maxTableSize = 1 << 14
		// tableMask is redundant, but helps the compiler eliminate bounds
		// checks.
		tableMask = maxTableSize - 1
	)
	shift, tableSize := uint32(32-8), 1<<8
	for tableSize < maxTableSize && tableSize < len(src) {
		shift--
		tableSize *= 2
	}
	var table [maxTableSize]uint16

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := len(src) - inputMargin

	// nextEmit is where in src the next emitLiteral should start from.
	nextEmit := 0

	// The encoded form must start with a literal, as there are no previous
	// bytes to copy, so we start looking for hash matches at s == 1.
	s := 1
	nextHash := hash(load32(src, s), shift)

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := 32

		nextS := s
		candidate := 0
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = int(table[nextHash&tableMask])
			table[nextHash&tableMask] = uint16(s)
			nextHash = hash(load32(src, nextS), shift)
			if load32(src, s) == load32(src, candidate) {
				break
			}
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		d += emitLiteral(dst[d:], src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.
			base := s
			s += 4
			for i := candidate + 4; s < len(src) && src[i] == src[s]; i, s = i+1, s+1 {
			}
			d += emitCopy(dst[d:], base-candidate, s-base)
			nextEmit = s
			if s >= sLimit {
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load64(src, s-1)
			prevHash := hash(uint32(x>>0), shift)
			table[prevHash&tableMask] = uint16(s - 1)
			currHash := hash(uint32(x>>8), shift)
			candidate = int(table[currHash&tableMask])
			table[currHash&tableMask] = uint16(s)
			if uint32(x>>8) != load32(src, candidate) {
				nextHash = hash(uint32(x>>16), shift)
				s++
				break
			}
		}
	}

emitRemainder:
	if nextEmit < len(src) {
		d += emitLiteral(dst[d:], src[nextEmit:])
	}
	return d
}

// MaxEncodedLen returns the maximum length of a snappy block, given its
// uncompressed length.
//
// It will return a negative value if srcLen is too large to encode.
func MaxEncodedLen(srcLen int) int {
	n := uint64(srcLen)
	if n > 0xffffffff {
		return -1
	}
	// Compressed data can be defined as:
	//    compressed := item* literal*
	//    item       := literal* copy
	//
	// The trailing literal sequence has a space blowup of at most 62/60
	// since a literal of length 60 needs one tag byte + one extra byte
	// for length information.
	//
	// Item blowup is trickier to measure. Suppose the "copy" op copies
	// 4 bytes of data. Because of a special check in the encoding code,
	// we produce a 4-byte copy only if the offset is < 65536. Therefore
	// the copy op takes 3 bytes to encode, and this type of item leads
	// to at most the 62/60 blowup for representing literals.
	//
	// Suppose the "copy" op copies 5 bytes of data. If the offset is big
	// enough, it will take 5 bytes to encode the copy op. Therefore the
	// worst case here is a one-byte literal followed by a five-byte copy.
	// That is, 6 bytes of input turn into 7 bytes of "compressed" data.
	//
	// This last factor dominates the blowup, so the final estimate is:
	n = 32 + n + n/6
	if n > 0xffffffff {
		return -1
	}
	return int(n)
}

var errClosed = errors.New("snappy: Writer is closed")

// NewWriter returns a new Writer that compresses to w.
//
// The Writer returned does not buffer writes. There is no need to Flush or
// Close such a Writer.
//
// Deprecated: the Writer returned is not suitable for many small writes, only
// for few large writes. Use NewBufferedWriter instead, which is efficient
// regardless of the frequency and shape of the writes, and remember to Close
// that Writer when done.
func NewWriter(w io.Writer) *Writer {
	return &Writer{
		w:    w,
		obuf: make([]byte, obufLen),
	}
}

// NewBufferedWriter returns a new Writer that compresses to w, using the
// framing format described at
// https://github.com/google/snappy/blob/master/framing_format.txt
//
// The Writer returned buffers writes. Users must call Close to guarantee all
// data has been forwarded to the underlying io.Writer. They may also call
// Flush zero or more times before calling Close.
func NewBufferedWriter(w io.Writer) *Writer {
	return &Writer{
		w:    w,
		ibuf: make([]byte, 0, maxBlockSize),
		obuf: make([]byte, obufLen),
	}
}

// Writer is an io.Writer than can write Snappy-compressed bytes.
type Writer struct {
	w   io.Writer
	err error

	// ibuf is a buffer for the incoming (uncompressed) bytes.
	//
	// Its use is optional. For backwards compatibility, Writers created by the
	// NewWriter function have ibuf == nil, do not buffer incoming bytes, and
	// therefore do not need to be Flush'ed or Close'd.
	ibuf []byte

	// obuf is a buffer for the outgoing (compressed) bytes.
	obuf []byte

	// wroteStreamHeader is whether we have written the stream header.
	wroteStreamHeader bool
}

// Reset discards the writer's state and switches the Snappy writer to write to
// w. This permits reusing a Writer rather than allocating a new one.
func (w *Writer) Reset(writer io.Writer) {
	w.w = writer
	w.err = nil
	if w.ibuf != nil {
		w.ibuf = w.ibuf[:0]
	}
	w.wroteStreamHeader = false
}

// Write satisfies the io.Writer interface.
func (w *Writer) Write(p []byte) (nRet int, errRet error) {
	if w.ibuf == nil {
		// Do not buffer incoming bytes. This does not perform or compress well
		// if the caller of Writer.Write writes many small slices. This
		// behavior is therefore deprecated, but still supported for backwards
		// compatibility with code that doesn't explicitly Flush or Close.
		return w.write(p)
	}

	// The remainder of this method is based on bufio.Writer.Write from the
	// standard library.

	for len(p) > (cap(w.ibuf)-len(w.ibuf)) && w.err == nil {
		var n int
		if len(w.ibuf) == 0 {
			// Large write, empty buffer.
			// Write directly from p to avoid copy.
			n, _ = w.write(p)
		} else {
			n = copy(w.ibuf[len(w.ibuf):cap(w.ibuf)], p)
			w.ibuf = w.ibuf[:len(w.ibuf)+n]
			w.Flush()
		}
		nRet += n
		p = p[n:]
	}
	if w.err != nil {
		return nRet, w.err
	}
	n := copy(w.ibuf[len(w.ibuf):cap(w.ibuf)], p)
	w.ibuf = w.ibuf[:len(w.ibuf)+n]
	nRet += n
	return nRet, nil
}

func (w *Writer) write(p []byte) (nRet int, errRet error) {
	if w.err != nil {
		return 0, w.err
	}
	for len(p) > 0 {
		obufStart := len(magicChunk)
		if !w.wroteStreamHeader {
			w.wroteStreamHeader = true
			copy(w.obuf, magicChunk)
			obufStart = 0
		}

		var uncompressed []byte
		if len(p) > maxBlockSize {
			uncompressed, p = p[:maxBlockSize], p[maxBlockSize:]
		} else {
			uncompressed, p = p, nil
		}
		checksum := crc(uncompressed)

		// Compress the buffer, discarding the result if the improvement
		// isn't at least 12.5%.
		compressed := Encode(w.obuf[obufHeaderLen:], uncompressed)
		chunkType := uint8(chunkTypeCompressedData)
		chunkLen := 4 + len(compressed)
		obufEnd := obufHeaderLen + len(compressed)
		if len(compressed) >= len(uncompressed)-len(uncompressed)/8 {
			chunkType = chunkTypeUncompressedData
			chunkLen = 4 + len(uncompressed)
			obufEnd = obufHeaderLen
		}

		// Fill in the per-chunk header that comes before the body.
		w.obuf[len(magicChunk)+0] = chunkType
		w.obuf[len(magicChunk)+1] = uint8(chunkLen >> 0)
		w.obuf[len(magicChunk)+2] = uint8(chunkLen >> 8)
		w.obuf[len(magicChunk)+3] = uint8(chunkLen >> 16)
		w.obuf[len(magicChunk)+4] = uint8(checksum >> 0)
		w.obuf[len(magicChunk)+5] = uint8(checksum >> 8)
		w.obuf[len(magicChunk)+6] = uint8(checksum >> 16)
		w.obuf[len(magicChunk)+7] = uint8(checksum >> 24)

		if _, err := w.w.Write(w.obuf[obufStart:obufEnd]); err != nil {
			w.err = err
			return nRet, err
		}
		if chunkType == chunkTypeUncompressedData {
			if _, err := w.w.Write(uncompressed); err != nil {
				w.err = err
				return nRet, err
			}
		}
		nRet += len(uncompressed)
	}
	return nRet, nil
}

// Flush flushes the Writer to its underlying io.Writer.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.ibuf) == 0 {
		return nil
	}
	w.write(w.ibuf)
	w.ibuf = w.ibuf[:0]
	return w.err
}

// Close calls Flush and then closes the Writer.
func (w *Writer) Close() error {
	w.Flush()
	ret := w.err
	if w.err == nil {
		w.err = errClosed
	}
	return ret
}
