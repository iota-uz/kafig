// Command wasm-merge-data coalesces the active data segments of a WebAssembly
// module.
//
// Wizer snapshots linear memory as thousands of small data segments (it splits
// at every run of zero bytes). Instantiating such a module costs one bounds
// check, constant-expression evaluation and copy per segment, which dominates
// wazero instantiation time. Segments whose gap is at most -max-gap bytes are
// merged into one, with the gap filled by zero bytes. Since linear memory
// starts zeroed, the initial memory image is unchanged.
//
// Only modules whose data segments are all active, target memory 0 and use an
// i32.const offset are supported; anything else is rejected rather than
// rewritten. The transformation is deterministic.
//
// Usage: wasm-merge-data [-max-gap N] <in.wasm> <out.wasm>
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
)

const (
	sectionData      = 11
	sectionDataCount = 12
)

type segment struct {
	offset uint32
	data   []byte
}

func main() {
	maxGap := flag.Uint("max-gap", 4096, "merge segments separated by at most this many zero bytes")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: wasm-merge-data [-max-gap N] <in.wasm> <out.wasm>")
		os.Exit(2)
	}
	in, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fatal(err)
	}
	out, before, after, err := rewrite(in, uint32(*maxGap))
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(flag.Arg(1), out, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wasm-merge-data: %d -> %d data segments\n", before, after)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "wasm-merge-data:", err)
	os.Exit(1)
}

func rewrite(in []byte, maxGap uint32) ([]byte, int, int, error) {
	if len(in) < 8 || !bytes.Equal(in[:4], []byte("\x00asm")) {
		return nil, 0, 0, errors.New("not a wasm module")
	}
	var out bytes.Buffer
	out.Write(in[:8])
	before, after := 0, 0
	pos := 8
	for pos < len(in) {
		id := in[pos]
		size, n, err := readU32(in[pos+1:])
		if err != nil {
			return nil, 0, 0, fmt.Errorf("section header at %d: %w", pos, err)
		}
		start := pos + 1 + n
		end := start + int(size)
		if end > len(in) {
			return nil, 0, 0, fmt.Errorf("section %d overruns module", id)
		}
		payload := in[start:end]
		switch id {
		case sectionData:
			segs, err := parseData(payload)
			if err != nil {
				return nil, 0, 0, err
			}
			before = len(segs)
			merged := merge(segs, maxGap)
			after = len(merged)
			payload = encodeData(merged)
		case sectionDataCount:
			// Rewritten after the data section is known; the data count
			// section precedes the code section, so compute it eagerly.
			segs, err := dataSegmentsOf(in)
			if err != nil {
				return nil, 0, 0, err
			}
			payload = appendU32(nil, uint32(len(merge(segs, maxGap))))
		}
		out.WriteByte(id)
		out.Write(appendU32(nil, uint32(len(payload))))
		out.Write(payload)
		pos = end
	}
	return out.Bytes(), before, after, nil
}

// dataSegmentsOf locates and parses the data section of a module.
func dataSegmentsOf(in []byte) ([]segment, error) {
	pos := 8
	for pos < len(in) {
		id := in[pos]
		size, n, err := readU32(in[pos+1:])
		if err != nil {
			return nil, err
		}
		start := pos + 1 + n
		if id == sectionData {
			return parseData(in[start : start+int(size)])
		}
		pos = start + int(size)
	}
	return nil, nil
}

func parseData(p []byte) ([]segment, error) {
	count, n, err := readU32(p)
	if err != nil {
		return nil, err
	}
	p = p[n:]
	segs := make([]segment, 0, count)
	for i := uint32(0); i < count; i++ {
		flags, n, err := readU32(p)
		if err != nil {
			return nil, err
		}
		p = p[n:]
		if flags != 0 {
			return nil, fmt.Errorf("data segment %d: unsupported flags %d (only active memory-0 segments)", i, flags)
		}
		if len(p) == 0 || p[0] != 0x41 { // i32.const
			return nil, fmt.Errorf("data segment %d: offset is not i32.const", i)
		}
		off, n, err := readS32(p[1:])
		if err != nil {
			return nil, err
		}
		p = p[1+n:]
		if len(p) == 0 || p[0] != 0x0b { // end
			return nil, fmt.Errorf("data segment %d: offset expression is not a single i32.const", i)
		}
		p = p[1:]
		size, n, err := readU32(p)
		if err != nil {
			return nil, err
		}
		p = p[n:]
		if uint32(len(p)) < size {
			return nil, fmt.Errorf("data segment %d overruns section", i)
		}
		segs = append(segs, segment{offset: uint32(off), data: p[:size]})
		p = p[size:]
	}
	if len(p) != 0 {
		return nil, errors.New("trailing bytes in data section")
	}
	return segs, nil
}

// merge applies the segments in their original order onto a sparse image
// (later segments win, as at instantiation) and emits coalesced runs.
func merge(segs []segment, maxGap uint32) []segment {
	if len(segs) == 0 {
		return nil
	}
	order := make([]int, len(segs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return segs[order[a]].offset < segs[order[b]].offset })

	// Group by offset into runs, then paint every member of a run onto a
	// buffer in original segment order so overlapping writes keep semantics.
	var out []segment
	runStart := 0
	runEnd := uint64(segs[order[0]].offset) + uint64(len(segs[order[0]].data))
	flush := func(members []int) {
		lo := segs[members[0]].offset
		hi := uint64(lo)
		for _, m := range members {
			if e := uint64(segs[m].offset) + uint64(len(segs[m].data)); e > hi {
				hi = e
			}
		}
		buf := make([]byte, hi-uint64(lo))
		sorted := append([]int(nil), members...)
		sort.Ints(sorted) // original order
		for _, m := range sorted {
			copy(buf[segs[m].offset-lo:], segs[m].data)
		}
		out = append(out, segment{offset: lo, data: buf})
	}
	for i := 1; i < len(order); i++ {
		s := segs[order[i]]
		if uint64(s.offset) <= runEnd+uint64(maxGap) {
			if e := uint64(s.offset) + uint64(len(s.data)); e > runEnd {
				runEnd = e
			}
			continue
		}
		flush(order[runStart:i])
		runStart = i
		runEnd = uint64(s.offset) + uint64(len(s.data))
	}
	flush(order[runStart:])
	return out
}

func encodeData(segs []segment) []byte {
	b := appendU32(nil, uint32(len(segs)))
	for _, s := range segs {
		b = append(b, 0x00, 0x41)
		b = appendS32(b, int32(s.offset))
		b = append(b, 0x0b)
		b = appendU32(b, uint32(len(s.data)))
		b = append(b, s.data...)
	}
	return b
}

func readU32(b []byte) (uint32, int, error) {
	var v uint32
	for i := 0; i < 5 && i < len(b); i++ {
		v |= uint32(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("bad u32 LEB128")
}

func readS32(b []byte) (int32, int, error) {
	var v int64
	var shift uint
	for i := 0; i < 5 && i < len(b); i++ {
		v |= int64(b[i]&0x7f) << shift
		shift += 7
		if b[i]&0x80 == 0 {
			if shift < 64 && b[i]&0x40 != 0 {
				v |= -1 << shift
			}
			return int32(v), i + 1, nil
		}
	}
	return 0, 0, errors.New("bad s32 LEB128")
}

func appendU32(b []byte, v uint32) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

func appendS32(b []byte, v int32) []byte {
	x := int64(v)
	for {
		c := byte(x & 0x7f)
		x >>= 7
		if (x == 0 && c&0x40 == 0) || (x == -1 && c&0x40 != 0) {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}
