package mp4_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// boxLayout describes one top-level box of a decoded file with its cumulative position.
type boxLayout struct {
	typ   string
	start uint64
	size  uint64
}

func layoutOf(f *mp4.File) []boxLayout {
	layout := make([]boxLayout, 0, len(f.Children))
	pos := uint64(0)
	for _, b := range f.Children {
		layout = append(layout, boxLayout{typ: b.Type(), start: pos, size: b.Size()})
		pos += b.Size()
	}
	return layout
}

func findBox(layout []boxLayout, typ string) boxLayout {
	for _, b := range layout {
		if b.typ == typ {
			return b
		}
	}
	return boxLayout{}
}

// newOffsetTrak builds a minimal self-contained trak carrying one stco or co64 table.
func newOffsetTrak(t *testing.T, trackID uint32, useCo64 bool, offsets []uint64) *mp4.TrakBox {
	t.Helper()
	trak := mp4.CreateEmptyTrak(trackID, 48000, "audio", "und")
	stbl := trak.Mdia.Minf.Stbl
	// An stts without entries marks the file as fragmented, so fill the box in place,
	// since the same pointer is also held in stbl.Children.
	stbl.Stts.SampleCount = []uint32{1}
	stbl.Stts.SampleTimeDelta = []uint32{1024}
	children := make([]mp4.Box, 0, len(stbl.Children))
	for _, c := range stbl.Children {
		if c.Type() != "stco" {
			children = append(children, c)
		}
	}
	stbl.Children = children
	stbl.Stco = nil
	var offBox mp4.Box
	if useCo64 {
		offBox = &mp4.Co64Box{ChunkOffset: offsets}
	} else {
		u32 := make([]uint32, len(offsets))
		for i, off := range offsets {
			u32[i] = uint32(off)
		}
		offBox = &mp4.StcoBox{ChunkOffset: u32}
	}
	stbl.AddChild(offBox)
	return trak
}

func newTestMoov(traks ...*mp4.TrakBox) *mp4.MoovBox {
	moov := mp4.NewMoovBox()
	moov.AddChild(mp4.CreateMvhd())
	for _, trak := range traks {
		moov.AddChild(trak)
	}
	return moov
}

func encodeBoxes(t *testing.T, boxes ...mp4.Box) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, b := range boxes {
		if err := b.Encode(&buf); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func decodeModes() []struct {
	name string
	opt  mp4.Option
} {
	return []struct {
		name string
		opt  mp4.Option
	}{
		{name: "in-memory", opt: nil},
		{name: "lazy", opt: mp4.WithDecodeMode(mp4.DecModeLazyMdat)},
	}
}

func decodeBytes(t *testing.T, data []byte, opt mp4.Option) *mp4.File {
	t.Helper()
	var (
		f   *mp4.File
		err error
	)
	if opt == nil {
		f, err = mp4.DecodeFile(bytes.NewReader(data))
	} else {
		f, err = mp4.DecodeFile(bytes.NewReader(data), opt)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFaststartMovesMoov checks the full rearrangement: moov goes right after
// ftyp while every other box keeps its bytes and relative order.
func TestFaststartMovesMoov(t *testing.T) {
	ftyp := mp4.NewFtyp("mp42", 0, []string{"mp42", "iso6"})
	free := mp4.NewFreeBox([]byte("free-space-data"))
	wide := mp4.NewWideBox()
	unknown := mp4.CreateUnknownBox("zzzz", 8+4, []byte{1, 2, 3, 4})
	payload := bytes.Repeat([]byte{0xab}, 100)
	mdat := &mp4.MdatBox{Data: append([]byte(nil), payload...)}
	emptyMdat := &mp4.MdatBox{}
	trak := newOffsetTrak(t, 1, false, []uint64{0, 0}) // offsets fixed once the layout is known
	moov := newTestMoov(trak)

	mdatStart := uint64(len(encodeBoxes(t, ftyp, free, wide, unknown)))
	payloadStart := mdatStart + mdat.HeaderSize()
	stbl := trak.Mdia.Minf.Stbl
	stbl.Stco.ChunkOffset = []uint32{uint32(payloadStart), uint32(payloadStart + 50)}

	input := encodeBoxes(t, ftyp, free, wide, unknown, mdat, emptyMdat, moov)
	inLayout := layoutOf(decodeBytes(t, input, nil))

	for _, mode := range decodeModes() {
		t.Run(mode.name, func(t *testing.T) {
			f := decodeBytes(t, input, mode.opt)
			var out bytes.Buffer
			if err := mp4.Faststart(f, bytes.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			output := out.Bytes()
			if len(output) != len(input) {
				t.Fatalf("output size %d, want %d (no size change without co64 upgrade)", len(output), len(input))
			}
			outFile := decodeBytes(t, output, nil)
			if outFile.IsFragmented() {
				t.Fatal("output is fragmented")
			}
			gotTypes := make([]string, 0, len(outFile.Children))
			for _, b := range outFile.Children {
				gotTypes = append(gotTypes, b.Type())
			}
			wantTypes := []string{"ftyp", "moov", "free", "wide", "zzzz", "mdat", "mdat"}
			if !equalStrings(gotTypes, wantTypes) {
				t.Errorf("box order %v, want %v", gotTypes, wantTypes)
			}
			// Every non-moov box is copied with its original bytes. The output order is
			// ftyp, moov, then every other input box in its original order.
			outLayout := layoutOf(outFile)
			passthroughLayout := make([]boxLayout, 0, len(inLayout))
			for _, inBox := range inLayout {
				if inBox.typ != "ftyp" && inBox.typ != "moov" {
					passthroughLayout = append(passthroughLayout, inBox)
				}
			}
			if !bytes.Equal(output[outLayout[0].start:outLayout[0].start+outLayout[0].size],
				input[inLayout[0].start:inLayout[0].start+inLayout[0].size]) {
				t.Error("ftyp bytes changed")
			}
			for i, inBox := range passthroughLayout {
				outBox := outLayout[i+2]
				if outBox.typ != inBox.typ {
					t.Errorf("box %d: got type %s, want %s", i+2, outBox.typ, inBox.typ)
				}
				if !bytes.Equal(output[outBox.start:outBox.start+outBox.size],
					input[inBox.start:inBox.start+inBox.size]) {
					t.Errorf("box %s bytes changed", inBox.typ)
				}
			}
			// The mdat payload is unchanged.
			outMdat := findBox(outLayout, "mdat")
			outPayloadStart := outMdat.start + outFile.Mdat.HeaderSize()
			if !bytes.Equal(output[outPayloadStart:outPayloadStart+uint64(len(payload))], payload) {
				t.Error("mdat payload changed")
			}
			// Chunk offsets are the original offsets shifted once.
			shift := outPayloadStart - payloadStart
			wantOffsets := []uint32{uint32(payloadStart + shift), uint32(payloadStart + 50 + shift)}
			if !equalU32(outFile.Moov.Traks[0].Mdia.Minf.Stbl.Stco.ChunkOffset, wantOffsets) {
				t.Errorf("stco offsets %v, want %v",
					outFile.Moov.Traks[0].Mdia.Minf.Stbl.Stco.ChunkOffset, wantOffsets)
			}
		})
	}
}

// TestFaststartLargeMdatHeader checks that a 16-byte large-size mdat header is
// kept verbatim and that the offset shift accounts for its width.
func TestFaststartLargeMdatHeader(t *testing.T) {
	ftyp := mp4.NewFtyp("mp42", 0, []string{"mp42"})
	payload := bytes.Repeat([]byte{0xcd}, 40)
	mdat := &mp4.MdatBox{Data: append([]byte(nil), payload...), LargeSize: true}
	if mdat.HeaderSize() != 16 {
		t.Fatal("test setup: expected 16-byte mdat header")
	}
	trak := newOffsetTrak(t, 1, false, []uint64{0, 0})
	moov := newTestMoov(trak)
	mdatStart := ftyp.Size()
	payloadStart := mdatStart + mdat.HeaderSize()
	stbl := trak.Mdia.Minf.Stbl
	stbl.Stco.ChunkOffset = []uint32{uint32(payloadStart), uint32(payloadStart + 20)}

	input := encodeBoxes(t, ftyp, mdat, moov)
	f := decodeBytes(t, input, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if !f.Mdat.LargeSize {
		t.Fatal("decoded mdat lost its large-size header flag")
	}
	var out bytes.Buffer
	if err := mp4.Faststart(f, bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	output := out.Bytes()
	outFile := decodeBytes(t, output, nil)
	outLayout := layoutOf(outFile)
	outMdat := findBox(outLayout, "mdat")
	if !outFile.Mdat.LargeSize || outFile.Mdat.HeaderSize() != 16 {
		t.Error("16-byte mdat header was not preserved")
	}
	inLayout := layoutOf(decodeBytes(t, input, nil))
	inMdat := findBox(inLayout, "mdat")
	if !bytes.Equal(output[outMdat.start:outMdat.start+outMdat.size],
		input[inMdat.start:inMdat.start+inMdat.size]) {
		t.Error("mdat raw bytes (header and payload) changed")
	}
	shift := (outMdat.start + 16) - (inMdat.start + 16)
	want := []uint32{uint32(payloadStart + shift), uint32(payloadStart + 20 + shift)}
	if !equalU32(outFile.Moov.Traks[0].Mdia.Minf.Stbl.Stco.ChunkOffset, want) {
		t.Errorf("stco offsets %v, want %v", outFile.Moov.Traks[0].Mdia.Minf.Stbl.Stco.ChunkOffset, want)
	}
}

// TestFaststartStcoToCo64 checks that a 32-bit overflow converts the stco box
// of the affected track to a co64 box and grows the file by 4 bytes per entry.
func TestFaststartStcoToCo64(t *testing.T) {
	ftyp := mp4.NewFtyp("mp42", 0, []string{"mp42"})
	mdat := &mp4.MdatBox{Data: []byte("media")}
	trak := newOffsetTrak(t, 1, false, []uint64{0xFFFFFFF0, math.MaxUint32})
	moov := newTestMoov(trak)
	input := encodeBoxes(t, ftyp, mdat, moov)
	origMoovSize := moov.Size()

	f := decodeBytes(t, input, nil)
	var out bytes.Buffer
	if err := mp4.Faststart(f, bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	output := out.Bytes()
	if uint64(len(output)) != uint64(len(input))+8 { // two entries, 4 extra bytes each
		t.Errorf("output size %d, want %d", len(output), len(input)+8)
	}
	outFile := decodeBytes(t, output, nil)
	stbl := outFile.Moov.Traks[0].Mdia.Minf.Stbl
	if stbl.Stco != nil {
		t.Error("stco box was not removed")
	}
	if stbl.Co64 == nil {
		t.Fatal("expected co64 box")
	}
	finalShift := origMoovSize + 8
	want := []uint64{0xFFFFFFF0 + finalShift, math.MaxUint32 + finalShift}
	if !equalU64(stbl.Co64.ChunkOffset, want) {
		t.Errorf("co64 offsets %v, want %v", stbl.Co64.ChunkOffset, want)
	}
	if outFile.Children[0].Type() != "ftyp" || outFile.Children[1].Type() != "moov" {
		t.Error("moov not placed directly after ftyp")
	}
}

// TestFaststartCascadeUpgrade checks that growing the moov box for one track
// can push another track past 32 bits, and that an existing co64 stays co64.
func TestFaststartCascadeUpgrade(t *testing.T) {
	ftyp := mp4.NewFtyp("mp42", 0, []string{"mp42"})
	mdat := &mp4.MdatBox{Data: []byte("media")}
	trakA := newOffsetTrak(t, 1, false, []uint64{0xFFFFFFF0, math.MaxUint32})
	trakB := newOffsetTrak(t, 2, false, []uint64{0}) // value set below from the moov size
	trakC := newOffsetTrak(t, 3, true, []uint64{1000})
	moov := newTestMoov(trakA, trakB, trakC)
	origMoovSize := moov.Size()
	// With the initial shift (the original moov size), the offset fits in 32
	// bits exactly. Once track A grows the moov by 8 bytes, it overflows.
	trakB.Mdia.Minf.Stbl.Stco.ChunkOffset = []uint32{uint32(math.MaxUint32 - origMoovSize)}

	input := encodeBoxes(t, ftyp, mdat, moov)
	f := decodeBytes(t, input, nil)
	var out bytes.Buffer
	if err := mp4.Faststart(f, bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	outFile := decodeBytes(t, out.Bytes(), nil)
	finalShift := origMoovSize + 8 + 4 // growth of track A (2 entries) and track B (1 entry)
	for _, tc := range []struct {
		trakIdx  int
		wantCo64 []uint64
	}{
		{trakIdx: 0, wantCo64: []uint64{0xFFFFFFF0 + finalShift, math.MaxUint32 + finalShift}},
		{trakIdx: 1, wantCo64: []uint64{math.MaxUint32 - origMoovSize + finalShift}},
		{trakIdx: 2, wantCo64: []uint64{1000 + finalShift}},
	} {
		stbl := outFile.Moov.Traks[tc.trakIdx].Mdia.Minf.Stbl
		if stbl.Stco != nil {
			t.Errorf("track %d still has an stco box", tc.trakIdx+1)
		}
		if stbl.Co64 == nil {
			t.Errorf("track %d has no co64 box", tc.trakIdx+1)
			continue
		}
		if !equalU64(stbl.Co64.ChunkOffset, tc.wantCo64) {
			t.Errorf("track %d co64 offsets %v, want %v", tc.trakIdx+1, stbl.Co64.ChunkOffset, tc.wantCo64)
		}
	}
}

// TestFaststartNaturalFile runs the conversion on the moov-last test asset and
// checks layout, raw boxes, offset arithmetic and readable sample data.
func TestFaststartNaturalFile(t *testing.T) {
	const inName = "testdata/bbb_prog_10s.mp4"
	rawInput, err := os.ReadFile(inName)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range decodeModes() {
		t.Run(mode.name, func(t *testing.T) {
			fd, err := os.Open(inName)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			var f *mp4.File
			if mode.opt == nil {
				f, err = mp4.DecodeFile(fd)
			} else {
				f, err = mp4.DecodeFile(fd, mode.opt)
			}
			if err != nil {
				t.Fatal(err)
			}
			inLayout := layoutOf(f)
			if findBox(inLayout, "moov").start < findBox(inLayout, "mdat").start {
				t.Fatal("test asset is not moov-last")
			}
			// Faststart modifies f in place, so snapshot the original offsets first.
			type origTable struct {
				isCo64 bool
				stco   []uint32
				co64   []uint64
			}
			origTables := make([]origTable, len(f.Moov.Traks))
			for i, trak := range f.Moov.Traks {
				stbl := trak.Mdia.Minf.Stbl
				switch {
				case stbl.Stco != nil:
					origTables[i] = origTable{isCo64: false, stco: append([]uint32(nil), stbl.Stco.ChunkOffset...)}
				case stbl.Co64 != nil:
					origTables[i] = origTable{isCo64: true, co64: append([]uint64(nil), stbl.Co64.ChunkOffset...)}
				}
			}
			var out bytes.Buffer
			if err := mp4.Faststart(f, fd, &out); err != nil {
				t.Fatal(err)
			}
			output := out.Bytes()
			outFile := decodeBytes(t, output, nil)
			outLayout := layoutOf(outFile)
			gotTypes := make([]string, 0, len(outLayout))
			for _, b := range outLayout {
				gotTypes = append(gotTypes, b.typ)
			}
			wantTypes := []string{"ftyp", "moov", "free", "mdat"}
			if !equalStrings(gotTypes, wantTypes) {
				t.Errorf("box order %v, want %v", gotTypes, wantTypes)
			}
			if outFile.IsFragmented() {
				t.Error("output is fragmented")
			}
			// ftyp and free keep their original bytes.
			for _, typ := range []string{"ftyp", "free"} {
				inBox := findBox(inLayout, typ)
				outBox := findBox(outLayout, typ)
				if !bytes.Equal(output[outBox.start:outBox.start+outBox.size],
					rawInput[inBox.start:inBox.start+inBox.size]) {
					t.Errorf("box %s bytes changed", typ)
				}
			}
			// The mdat header width and payload are unchanged.
			inMdat := findBox(inLayout, "mdat")
			outMdat := findBox(outLayout, "mdat")
			if outMdat.size != inMdat.size {
				t.Errorf("mdat size %d, want %d", outMdat.size, inMdat.size)
			}
			inPayload := rawInput[inMdat.start+8 : inMdat.start+inMdat.size]
			outPayload := output[outMdat.start+8 : outMdat.start+outMdat.size]
			if !bytes.Equal(inPayload, outPayload) {
				t.Error("mdat payload changed")
			}
			// Every chunk offset is the original one shifted once, and stays in stco boxes.
			shift := (outMdat.start + 8) - (inMdat.start + 8)
			for i, orig := range origTables {
				outStbl := outFile.Moov.Traks[i].Mdia.Minf.Stbl
				if orig.isCo64 || outStbl.Stco == nil {
					t.Fatalf("track %d: expected stco boxes on both sides", i+1)
				}
				if len(orig.stco) != len(outStbl.Stco.ChunkOffset) {
					t.Fatalf("track %d: chunk count changed", i+1)
				}
				for j, off := range orig.stco {
					want := uint64(off) + shift
					if want > math.MaxUint32 {
						t.Fatalf("track %d chunk %d offset overflow not handled", i+1, j)
					}
					if uint64(outStbl.Stco.ChunkOffset[j]) != want {
						t.Errorf("track %d chunk %d: offset %d, want %d", i+1, j, outStbl.Stco.ChunkOffset[j], want)
					}
				}
			}
			// The sample data reachable through the new tables match the input samples.
			inMem := decodeBytes(t, rawInput, nil)
			for i, trak := range outFile.Moov.Traks {
				nrSamples := trak.GetNrSamples()
				var want, got bytes.Buffer
				if err := inMem.CopySampleData(&want, nil, inMem.Moov.Traks[i], 1, nrSamples, nil); err != nil {
					t.Fatal(err)
				}
				if err := outFile.CopySampleData(&got, nil, trak, 1, nrSamples, nil); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(want.Bytes(), got.Bytes()) {
					t.Errorf("track %d sample data differ", i+1)
				}
			}
		})
	}
}

// TestFaststartPassthrough checks that a file whose moov already precedes the
// non-empty mdat is emitted byte-for-byte, whatever else the layout contains.
func TestFaststartPassthrough(t *testing.T) {
	const inName = "testdata/prog_8s.mp4"
	rawInput, err := os.ReadFile(inName)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range decodeModes() {
		t.Run(mode.name, func(t *testing.T) {
			fd, err := os.Open(inName)
			if err != nil {
				t.Fatal(err)
			}
			defer fd.Close()
			var f *mp4.File
			if mode.opt == nil {
				f, err = mp4.DecodeFile(fd)
			} else {
				f, err = mp4.DecodeFile(fd, mode.opt)
			}
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := mp4.Faststart(f, fd, &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), rawInput) {
				t.Error("moov-first file was not copied unchanged")
			}
		})
	}
}

// TestFaststartErrors checks the unsupported-file errors of the library function.
func TestFaststartErrors(t *testing.T) {
	ftyp := mp4.NewFtyp("mp42", 0, []string{"mp42"})
	mdat := &mp4.MdatBox{Data: []byte("media")}
	trak := newOffsetTrak(t, 1, false, []uint64{100, 200})
	moov := newTestMoov(trak)
	validInput := encodeBoxes(t, ftyp, mdat, moov)

	t.Run("nil File", func(t *testing.T) {
		if err := mp4.Faststart(nil, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error for nil File")
		}
	})
	t.Run("nil ReadSeeker", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		if err := mp4.Faststart(f, nil, &bytes.Buffer{}); err == nil {
			t.Error("expected error for nil ReadSeeker")
		}
	})
	t.Run("nil Writer", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		if err := mp4.Faststart(f, bytes.NewReader(validInput), nil); err == nil {
			t.Error("expected error for nil Writer")
		}
	})
	t.Run("track without stbl", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		f.Moov.Traks[0].Mdia.Minf.Stbl = nil
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error for track without stbl")
		}
	})
	t.Run("fragmented", func(t *testing.T) {
		fd, err := os.Open("testdata/prog_8s_dec_dashinit.mp4")
		if err != nil {
			t.Fatal(err)
		}
		defer fd.Close()
		f, err := mp4.DecodeFile(fd, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
		if err != nil {
			t.Fatal(err)
		}
		if err := mp4.Faststart(f, fd, &bytes.Buffer{}); err == nil {
			t.Error("expected error for fragmented file")
		}
	})
	t.Run("external data reference", func(t *testing.T) {
		fd, err := os.Open("testdata/prog_8s_dref.mp4")
		if err != nil {
			t.Fatal(err)
		}
		defer fd.Close()
		f, err := mp4.DecodeFile(fd)
		if err != nil {
			t.Fatal(err)
		}
		if err := mp4.Faststart(f, fd, &bytes.Buffer{}); err == nil {
			t.Error("expected error for external data reference")
		}
	})
	t.Run("external dref while mdat is present", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		stbl := f.Moov.Traks[0].Mdia.Minf.Stbl
		// A sample entry whose data_reference_index points at dref entry 1.
		entryPayload := make([]byte, 8)
		binary.BigEndian.PutUint16(entryPayload[6:8], 1)
		stbl.Stsd.AddChild(mp4.CreateUnknownBox("xxxx", 8+8, entryPayload))
		url := f.Moov.Traks[0].Mdia.Minf.Dinf.Dref.Children[0].(*mp4.URLBox)
		url.Flags = 0
		url.Location = "external-media.mp4"
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error for an external data reference")
		}
	})
	t.Run("external dref not hidden by moov-first passthrough", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/prog_8s.mp4")
		if err != nil {
			t.Fatal(err)
		}
		f := decodeBytes(t, raw, nil)
		url := f.Moov.Traks[0].Mdia.Minf.Dinf.Dref.Children[0].(*mp4.URLBox)
		url.Flags = 0
		url.Location = "external-media.mp4"
		if err := mp4.Faststart(f, bytes.NewReader(raw), &bytes.Buffer{}); err == nil {
			t.Error("expected external data reference error even with moov first")
		}
	})
	t.Run("two non-empty mdat boxes", func(t *testing.T) {
		f := mp4.NewFile()
		f.AddChild(ftyp, 0)
		f.AddChild(&mp4.MdatBox{Data: []byte("first")}, 0)
		f.AddChild(moov, 0)
		f.AddChild(&mp4.MdatBox{Data: []byte("second")}, 0)
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error for two non-empty mdat boxes")
		}
	})
	t.Run("missing ftyp", func(t *testing.T) {
		f := mp4.NewFile()
		f.AddChild(mdat, 0)
		f.AddChild(moov, 0)
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error without ftyp")
		}
	})
	t.Run("missing moov", func(t *testing.T) {
		f := mp4.NewFile()
		f.AddChild(ftyp, 0)
		f.AddChild(mdat, 0)
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error without moov")
		}
	})
	t.Run("no chunk offset table", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		stbl := f.Moov.Traks[0].Mdia.Minf.Stbl
		newChildren := make([]mp4.Box, 0, len(stbl.Children))
		for _, c := range stbl.Children {
			if c.Type() != "stco" {
				newChildren = append(newChildren, c)
			}
		}
		stbl.Children = newChildren
		stbl.Stco = nil
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error without stco/co64")
		}
	})
	t.Run("both stco and co64", func(t *testing.T) {
		f := decodeBytes(t, validInput, nil)
		stbl := f.Moov.Traks[0].Mdia.Minf.Stbl
		stbl.AddChild(&mp4.Co64Box{ChunkOffset: []uint64{100}})
		if err := mp4.Faststart(f, bytes.NewReader(validInput), &bytes.Buffer{}); err == nil {
			t.Error("expected error with both stco and co64")
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
