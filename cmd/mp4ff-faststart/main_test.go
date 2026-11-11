package main

import (
	"bytes"
	"io"
	"os"
	"path"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

func TestCommandLines(t *testing.T) {
	goodInFile := "../../mp4/testdata/bbb_prog_10s.mp4"
	fragInFile := "../../mp4/testdata/prog_8s_dec_dashinit.mp4"
	drefInFile := "../../mp4/testdata/prog_8s_dref.mp4"
	outFile := path.Join(t.TempDir(), "out.mp4")
	cases := []struct {
		desc string
		args []string
		err  bool
	}{
		{desc: "no args", args: []string{appName}, err: true},
		{desc: "unknown flag", args: []string{appName, "-x", goodInFile, outFile}, err: true},
		{desc: "missing outFile", args: []string{appName, goodInFile}, err: true},
		{desc: "too many files", args: []string{appName, goodInFile, outFile, "extra"}, err: true},
		{desc: "missing input file", args: []string{appName, "no-such-file.mp4", outFile}, err: true},
		{desc: "fragmented input", args: []string{appName, fragInFile, outFile}, err: true},
		{desc: "external data reference", args: []string{appName, drefInFile, outFile}, err: true},
		{desc: "version", args: []string{appName, "-version"}, err: false},
		{desc: "help", args: []string{appName, "-h"}, err: false},
		{desc: "moov moved to front", args: []string{appName, goodInFile, outFile}, err: false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			err := run(c.args, io.Discard)
			if c.err && err == nil {
				t.Error("expected error but got nil")
			}
			if !c.err && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestFaststartOutputHasMoovFirst(t *testing.T) {
	inFile := "../../mp4/testdata/bbb_prog_10s.mp4"
	outFile := path.Join(t.TempDir(), "out.mp4")
	if err := run([]string{appName, inFile, outFile}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	outMp4, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if outMp4.IsFragmented() {
		t.Error("output is fragmented")
	}
	if outMp4.Ftyp == nil || outMp4.Moov == nil || outMp4.Mdat == nil {
		t.Fatal("output lacks ftyp, moov or mdat")
	}
	if outMp4.Children[0].Type() != "ftyp" || outMp4.Children[1].Type() != "moov" {
		t.Error("moov was not placed directly after ftyp")
	}
	pos := uint64(0)
	moovPos, mdatPos := uint64(0), uint64(0)
	for _, b := range outMp4.Children {
		switch b.Type() {
		case "moov":
			moovPos = pos
		case "mdat":
			if b.Size()-8 > 0 {
				mdatPos = pos
			}
		}
		pos += b.Size()
	}
	if moovPos > mdatPos {
		t.Error("moov is still behind the non-empty mdat")
	}
}

func TestFaststartPassthroughIsByteIdentical(t *testing.T) {
	inFile := "../../mp4/testdata/prog_8s.mp4"
	rawInput, err := os.ReadFile(inFile)
	if err != nil {
		t.Fatal(err)
	}
	outFile := path.Join(t.TempDir(), "out.mp4")
	if err := run([]string{appName, inFile, outFile}, io.Discard); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, rawInput) {
		t.Error("moov-first input was not copied unchanged")
	}
}
