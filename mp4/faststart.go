package mp4

import (
	"fmt"
	"io"
	"math"
)

// Faststart moves the moov box of a progressive (non-fragmented) file f,
// decoded from rs, in front of its media data and writes the result to w.
// A player can then start playback from the beginning of the file without
// first reading to its end.
//
// Only the baseline case is supported: one ftyp box, one moov box, the ftyp
// first, and exactly one non-empty mdat box, whose media chunk positions are
// given by the stco or co64 boxes of the tracks. Fragmented files, files with
// more than one non-empty mdat box, and tracks whose sample data reside
// outside the file through a data reference, are rejected with an error.
// Other offset tables of protected media are not handled.
//
// When the moov box already precedes the non-empty mdat box, the complete
// input is copied byte-for-byte, regardless of anything else in the layout.
// Otherwise the moov box is written directly after the ftyp box, while every
// other top-level box, including free, skip, wide, empty mdat and unknown
// boxes, keeps its original bytes and relative order, and the mdat box keeps
// its header width and payload. The chunk offsets are rewritten from their
// original values and the final layout in one step, so a shift is never
// accumulated twice.
//
// A track whose stco entries do not fit in 32 bits after the shift is
// converted to a co64 box. Such a conversion grows the moov box, which moves
// the mdat box further and may push another track past 32 bits, so the
// conversions are decided in a fixed-point loop. Tracks that already use co64
// keep it. f is modified in place: its chunk offset tables carry the output
// values and stco boxes converted to co64 are replaced. A buffered writer
// should be used when w is a file, since the moov box is made of many small
// boxes, each encoded with one write call.
func Faststart(f *File, rs io.ReadSeeker, w io.Writer) error {
	if f == nil {
		return fmt.Errorf("faststart: nil File")
	}
	if rs == nil {
		return fmt.Errorf("faststart: nil ReadSeeker")
	}
	if w == nil {
		return fmt.Errorf("faststart: nil Writer")
	}
	if f.IsFragmented() {
		return fmt.Errorf("faststart: fragmented files are not supported")
	}
	if f.Ftyp == nil || f.Moov == nil {
		return fmt.Errorf("faststart: file must have one ftyp box and one moov box")
	}

	// Locate the required top-level boxes and the original start position of
	// every box. The boxes of a decoded file are contiguous from position 0,
	// and each decoded Size is its on-disk size, also for lazily decoded boxes.
	nrFtyp, nrMoov, nrNonEmptyMdat := 0, 0, 0
	ftypIdx, moovIdx, mdatIdx := -1, -1, -1
	type boxPos struct {
		box   Box
		start uint64
		size  uint64
	}
	boxes := make([]boxPos, 0, len(f.Children))
	pos := uint64(0)
	for i, b := range f.Children {
		bp := boxPos{box: b, start: pos, size: b.Size()}
		boxes = append(boxes, bp)
		switch b.Type() {
		case "ftyp":
			nrFtyp++
			ftypIdx = i
		case "moov":
			nrMoov++
			moovIdx = i
		case "mdat":
			mdat := b.(*MdatBox)
			if mdat.Size()-mdat.HeaderSize() > 0 {
				nrNonEmptyMdat++
				mdatIdx = i
			}
		}
		pos += b.Size()
	}
	if nrFtyp != 1 || ftypIdx != 0 {
		return fmt.Errorf("faststart: expected exactly one ftyp box as the first box")
	}
	if nrMoov != 1 {
		return fmt.Errorf("faststart: expected exactly one moov box, got %d", nrMoov)
	}
	if nrNonEmptyMdat != 1 {
		return fmt.Errorf("faststart: expected exactly one non-empty mdat box, got %d", nrNonEmptyMdat)
	}

	// The unsupported-file checks are independent of the box order, so they run
	// before the moov-first shortcut below.
	tracks, err := faststartTracks(f.Moov)
	if err != nil {
		return err
	}

	// The moov box already being in front of the media data takes priority
	// over every normalization: the file passes through unchanged.
	if moovIdx < mdatIdx {
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("faststart: seek to start: %w", err)
		}
		if _, err := io.Copy(w, rs); err != nil {
			return fmt.Errorf("faststart: copy input: %w", err)
		}
		return nil
	}

	mdat := f.Children[mdatIdx].(*MdatBox)
	origMdatStart := boxes[mdatIdx].start
	origPayloadStart := origMdatStart + mdat.HeaderSize()
	ftypSize := boxes[ftypIdx].size

	// Sizes of the boxes that keep both their bytes and their place relative
	// to the mdat box: everything before it except ftyp and moov. In this
	// branch the moov box is behind the mdat box, so only ftyp is filtered out.
	var prefixSize uint64
	for i := 0; i < mdatIdx; i++ {
		if i == ftypIdx || i == moovIdx {
			continue
		}
		prefixSize += boxes[i].size
	}
	origMoovSize := boxes[moovIdx].size

	// Decide which stco boxes must become co64 boxes. Growing the moov box
	// shifts the mdat box, which may overflow further stco boxes, so the
	// decision is repeated until no more track overflows. The final offsets
	// are always derived from the original offsets plus one final shift.
	moovGrowth := uint64(0)
	upgraded := make(map[*StblBox]bool)
	for {
		newMoovSize := origMoovSize + moovGrowth
		finalMdatStart := ftypSize + newMoovSize + prefixSize
		shift := finalMdatStart + mdat.HeaderSize() - origPayloadStart
		changed := false
		for _, t := range tracks {
			if t.isCo64 || upgraded[t.stbl] {
				continue
			}
			for _, off := range t.origOffsets {
				if off+shift > math.MaxUint32 {
					upgraded[t.stbl] = true
					// Every entry grows from 4 to 8 bytes. The box header and
					// the version, flags and entry-count fields keep their size.
					moovGrowth += 4 * uint64(len(t.origOffsets))
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}

	newMoovSize := origMoovSize + moovGrowth
	finalMdatStart := ftypSize + newMoovSize + prefixSize
	shift := finalMdatStart + mdat.HeaderSize() - origPayloadStart

	// Rewrite the chunk offset tables once, from the original values and the
	// single final shift. stco boxes marked for it become co64 boxes here.
	for _, t := range tracks {
		newOffsets := make([]uint64, len(t.origOffsets))
		for i, off := range t.origOffsets {
			newOffsets[i] = off + shift
		}
		switch {
		case t.isCo64:
			t.stbl.Co64.ChunkOffset = newOffsets
		case upgraded[t.stbl]:
			co64 := &Co64Box{
				Version:     t.stbl.Stco.Version,
				Flags:       t.stbl.Stco.Flags,
				ChunkOffset: newOffsets,
			}
			for i, child := range t.stbl.Children {
				if child == t.stbl.Stco {
					t.stbl.Children[i] = co64
					break
				}
			}
			t.stbl.Co64 = co64
			t.stbl.Stco = nil
		default:
			for i, off := range newOffsets {
				t.stbl.Stco.ChunkOffset[i] = uint32(off)
			}
		}
	}

	// Write ftyp, then the rewritten moov box, then all remaining boxes in
	// their original relative order and with their original bytes. The raw
	// copies keep the mdat header width (8 or 16 bytes) and every free, skip,
	// wide, empty mdat or unknown box unchanged.
	if err := copyBoxRange(rs, w, boxes[ftypIdx].start, ftypSize); err != nil {
		return err
	}
	if err := f.Moov.Encode(w); err != nil {
		return fmt.Errorf("faststart: encode moov: %w", err)
	}
	for i := range f.Children {
		if i == ftypIdx || i == moovIdx {
			continue
		}
		if err := copyBoxRange(rs, w, boxes[i].start, boxes[i].size); err != nil {
			return err
		}
	}
	return nil
}

// faststartTrack is the chunk offset table of one track, with its original offsets.
type faststartTrack struct {
	stbl        *StblBox
	isCo64      bool
	origOffsets []uint64
}

// faststartTracks collects the chunk offset table of every track of moov and
// rejects tracks whose data are outside this file or that lack a single
// stco or co64 box.
func faststartTracks(moov *MoovBox) ([]faststartTrack, error) {
	tracks := make([]faststartTrack, 0, len(moov.Traks))
	for trackNr, trak := range moov.Traks {
		if err := trak.CheckDataIsSelfContained(); err != nil {
			return nil, fmt.Errorf("faststart: %w", err)
		}
		if trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			return nil, fmt.Errorf("faststart: track %d has no stbl box", trackNr+1)
		}
		stbl := trak.Mdia.Minf.Stbl
		switch {
		case stbl.Stco != nil && stbl.Co64 != nil:
			return nil, fmt.Errorf("faststart: track %d has both stco and co64 boxes", trackNr+1)
		case stbl.Stco != nil:
			orig := make([]uint64, len(stbl.Stco.ChunkOffset))
			for i, off := range stbl.Stco.ChunkOffset {
				orig[i] = uint64(off)
			}
			tracks = append(tracks, faststartTrack{stbl: stbl, isCo64: false, origOffsets: orig})
		case stbl.Co64 != nil:
			orig := make([]uint64, len(stbl.Co64.ChunkOffset))
			copy(orig, stbl.Co64.ChunkOffset)
			tracks = append(tracks, faststartTrack{stbl: stbl, isCo64: true, origOffsets: orig})
		default:
			return nil, fmt.Errorf("faststart: track %d has neither stco nor co64 box", trackNr+1)
		}
	}
	return tracks, nil
}

// copyBoxRange copies size bytes starting at start from rs to w.
func copyBoxRange(rs io.ReadSeeker, w io.Writer, start, size uint64) error {
	if start > math.MaxInt64 || size > math.MaxInt64 {
		return fmt.Errorf("faststart: box range at %d with size %d is too large", start, size)
	}
	if _, err := rs.Seek(int64(start), io.SeekStart); err != nil {
		return fmt.Errorf("faststart: seek to %d: %w", start, err)
	}
	if _, err := io.CopyN(w, rs, int64(size)); err != nil {
		return fmt.Errorf("faststart: copy %d bytes at %d: %w", size, start, err)
	}
	return nil
}
