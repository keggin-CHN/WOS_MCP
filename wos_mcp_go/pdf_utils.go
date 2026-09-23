package main

import (
	"bytes"
	"fmt"
	"io"
)

var pdfEOFMarker = []byte("%%EOF")

// pdfLogicalEnd returns the byte immediately after the last PDF EOF marker.
// Some CNKI responses append a small WebFastLoad metadata block after %%EOF;
// the PDF itself is still usable, but strict readers reject the extra bytes.
func pdfLogicalEnd(r io.ReaderAt, size int64) (int64, error) {
	if size <= 0 {
		return 0, fmt.Errorf("empty PDF")
	}

	const chunkSize int64 = 64 << 10
	probeSize := size
	if probeSize > chunkSize {
		probeSize = chunkSize
	}
	probeStart := size - probeSize
	probe := make([]byte, probeSize)
	n, err := r.ReadAt(probe, probeStart)
	if err != nil && err != io.EOF {
		return 0, err
	}
	probe = probe[:n]
	trimmed := bytes.TrimRight(probe, "\r\n\t ")
	if idx := bytes.LastIndex(trimmed, pdfEOFMarker); idx >= 0 && idx+len(pdfEOFMarker) == len(trimmed) {
		// Keep ordinary whitespace after %%EOF. The parser accepts it, and
		// preserving it avoids rewriting otherwise normal PDFs.
		return size, nil
	}

	// Scan backwards in bounded chunks so a large trailing block does not
	// require loading the complete document into memory. Keep the first bytes
	// of the later chunk to catch a marker split across the chunk boundary.
	var overlap []byte
	end := size
	for end > 0 {
		start := end - chunkSize
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		n, err := r.ReadAt(chunk, start)
		if err != nil && err != io.EOF {
			return 0, err
		}
		chunk = chunk[:n]

		scan := chunk
		if len(overlap) > 0 {
			scan = append(append([]byte(nil), chunk...), overlap...)
		}
		for idx := bytes.LastIndex(scan, pdfEOFMarker); idx >= 0; {
			// A match beginning in overlap was already covered by the later
			// chunk. Only accept markers that start in this chunk.
			if idx < len(chunk) {
				markerEnd := start + int64(idx+len(pdfEOFMarker))
				return pdfTrailingWhitespaceEnd(r, size, markerEnd)
			}
			if idx == 0 {
				break
			}
			idx = bytes.LastIndex(scan[:idx], pdfEOFMarker)
		}

		keep := len(chunk)
		if keep > len(pdfEOFMarker)-1 {
			keep = len(pdfEOFMarker) - 1
		}
		overlap = append([]byte(nil), chunk[:keep]...)
		end = start
	}
	return 0, fmt.Errorf("missing %%EOF")
}

func pdfTrailingWhitespaceEnd(r io.ReaderAt, size, markerEnd int64) (int64, error) {
	const chunkSize int64 = 64 << 10
	pos := markerEnd
	sawWhitespace := false
	for pos < size {
		n := size - pos
		if n > chunkSize {
			n = chunkSize
		}
		chunk := make([]byte, n)
		readN, err := r.ReadAt(chunk, pos)
		if err != nil && err != io.EOF {
			return 0, err
		}
		for i, b := range chunk[:readN] {
			switch b {
			case ' ', '\t', '\r', '\n':
				sawWhitespace = true
			default:
				if sawWhitespace {
					// Keep one conventional line break, but discard duplicate
					// separators that CNKI sometimes adds before its metadata.
					return min(markerEnd+1, size), nil
				}
				return pos + int64(i), nil
			}
		}
		pos += int64(readN)
		if readN == 0 {
			break
		}
	}
	if sawWhitespace {
		return min(markerEnd+1, size), nil
	}
	return pos, nil
}
