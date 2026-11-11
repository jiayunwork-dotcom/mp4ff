package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Eyevinn/mp4ff/internal"
	"github.com/Eyevinn/mp4ff/mp4"
)

const (
	appName = "mp4ff-faststart"
)

var usg = `%s moves the moov box of a progressive (non-fragmented) mp4 file in
front of the media data, so that playback can start from the beginning of the
file. All media data is copied unchanged.

Usage of %s:
`

type options struct {
	version bool
}

func parseOptions(fs *flag.FlagSet, args []string) (*options, error) {
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, usg, appName, appName)
		fmt.Fprintf(os.Stderr, "\n%s [options] <inFile> <outFile>\n\noptions:\n", appName)
		fs.PrintDefaults()
	}

	opts := options{}

	fs.BoolVar(&opts.version, "version", false, "Get mp4ff version")

	err := fs.Parse(args[1:])
	return &opts, err
}

func main() {
	if err := run(os.Args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	o, err := parseOptions(fs, args)

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if o.version {
		fmt.Fprintf(stdout, "%s %s\n", appName, internal.Version())
		return nil
	}

	if len(fs.Args()) != 2 {
		fs.Usage()
		return fmt.Errorf("must specify inFile and outFile")
	}

	inFilePath := fs.Arg(0)
	outFilePath := fs.Arg(1)

	ifh, err := os.Open(inFilePath)
	if err != nil {
		return fmt.Errorf("error opening input file: %w", err)
	}
	defer ifh.Close()
	parsedMp4, err := mp4.DecodeFile(ifh, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return fmt.Errorf("error decoding mp4 file: %w", err)
	}

	ofh, err := os.Create(outFilePath)
	if err != nil {
		return fmt.Errorf("error creating output file: %w", err)
	}
	defer ofh.Close()
	bufWriter := bufio.NewWriter(ofh)

	if err := mp4.Faststart(parsedMp4, ifh, bufWriter); err != nil {
		return fmt.Errorf("error moving moov box to front: %w", err)
	}
	if err := bufWriter.Flush(); err != nil {
		return fmt.Errorf("error writing output file: %w", err)
	}
	return nil
}
