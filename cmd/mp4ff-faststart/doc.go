/*
mp4ff-faststart moves the moov box of a progressive (non-fragmented) mp4 file
in front of the media data, so that playback can start from the beginning of
the file. All media data is copied unchanged.

Usage of mp4ff-faststart:

mp4ff-faststart [options] <inFile> <outFile>

options:

	-version
		Get mp4ff version
*/
package main
