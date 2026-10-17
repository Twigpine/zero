package tools

import (
	"bufio"
	"bytes"
	"io"
)

// readRawLine reads one line (including the trailing '\n' when present).
// On EOF with a non-empty unterminated buffer it returns that buffer with
// ended=false and err=nil. On EOF with an empty buffer it returns io.EOF.
func readRawLine(reader *bufio.Reader) ([]byte, bool, error) {
	line, ended, _, _, _, err := readRawLineLimited(reader, 0)
	return line, ended, err
}

// readRawLineLimited is like readRawLine but, when maxKeep > 0, retains at most
// maxKeep bytes of the line (including a trailing newline only if it still fits).
// Further bytes until the next newline are discarded so a multi-megabyte
// minified line cannot force a multi-megabyte allocation. clipped is true when
// any trailing content was discarded.
func readRawLineLimited(reader *bufio.Reader, maxKeep int) (line []byte, ended bool, clipped, containsNUL bool, bytesScanned int, err error) {
	if maxKeep <= 0 {
		line, ended, clipped, err := readRawLineUnlimited(reader)
		return line, ended, clipped, bytes.IndexByte(line, 0) >= 0, len(line), err
	}
	var kept []byte
	for {
		fragment, readErr := reader.ReadSlice('\n')
		bytesScanned += len(fragment)
		containsNUL = containsNUL || bytes.IndexByte(fragment, 0) >= 0
		if len(fragment) > 0 {
			room := maxKeep - len(kept)
			if room <= 0 {
				if fragment[len(fragment)-1] == '\n' {
					// Once maxKeep is full, a fragment containing only the
					// line break means no line content was discarded.
					if normalized, onlyLineBreak := trimDiscardedLineBreak(kept, fragment); onlyLineBreak {
						return normalized, true, false, containsNUL, bytesScanned, nil
					}
					return kept, true, true, containsNUL, bytesScanned, nil
				}
				if readErr != nil && readErr != bufio.ErrBufferFull && readErr != io.EOF {
					return kept, false, true, containsNUL, bytesScanned, readErr
				}
				discardedNUL, discardedBytes, dErr := discardThroughNewline(reader)
				bytesScanned += discardedBytes
				if dErr != nil && dErr != io.EOF {
					return kept, false, true, containsNUL || discardedNUL, bytesScanned, dErr
				}
				return kept, dErr == nil, true, containsNUL || discardedNUL, bytesScanned, nil
			}
			if len(fragment) <= room {
				if kept != nil || readErr == bufio.ErrBufferFull {
					kept = append(kept, fragment...)
				} else {
					kept = fragment
				}
			} else {
				kept = append(kept, fragment[:room]...)
				rest := fragment[room:]
				// Discarding only the line break is not content clipping.
				if normalized, onlyLineBreak := trimDiscardedLineBreak(kept, rest); onlyLineBreak {
					return normalized, true, false, containsNUL, bytesScanned, nil
				}
				if fragment[len(fragment)-1] == '\n' {
					// Non-newline content past maxKeep was discarded; line ended.
					return kept, true, true, containsNUL, bytesScanned, nil
				}
				if readErr != nil && readErr != bufio.ErrBufferFull && readErr != io.EOF {
					return kept, false, true, containsNUL, bytesScanned, readErr
				}
				discardedTailNUL, discardedBytes, dErr := discardThroughNewline(reader)
				bytesScanned += discardedBytes
				if dErr != nil && dErr != io.EOF {
					return kept, false, true, containsNUL || discardedTailNUL, bytesScanned, dErr
				}
				return kept, dErr == nil, true, containsNUL || discardedTailNUL, bytesScanned, nil
			}
		}
		switch readErr {
		case nil:
			return kept, true, false, containsNUL, bytesScanned, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(kept) > 0 {
				return kept, false, false, containsNUL, bytesScanned, nil
			}
			return nil, false, false, containsNUL, bytesScanned, io.EOF
		default:
			return nil, false, false, containsNUL, bytesScanned, readErr
		}
	}
}

func trimDiscardedLineBreak(kept, rest []byte) ([]byte, bool) {
	if len(rest) == 2 && rest[0] == '\r' && rest[1] == '\n' {
		return kept, true
	}
	if len(rest) != 1 || rest[0] != '\n' {
		return kept, false
	}
	if len(kept) > 0 && kept[len(kept)-1] == '\r' {
		kept = kept[:len(kept)-1]
	}
	return kept, true
}

func readRawLineUnlimited(reader *bufio.Reader) ([]byte, bool, bool, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if line != nil || err == bufio.ErrBufferFull {
				line = append(line, fragment...)
			} else {
				line = fragment
			}
		}
		switch err {
		case nil:
			return line, true, false, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) > 0 {
				return line, false, false, nil
			}
			return nil, false, false, io.EOF
		default:
			return nil, false, false, err
		}
	}
}

// discardThroughNewline drops input until a newline or EOF and reports whether
// any discarded fragment contained a NUL byte.
func discardThroughNewline(reader *bufio.Reader) (bool, int, error) {
	containsNUL := false
	bytesScanned := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		bytesScanned += len(fragment)
		containsNUL = containsNUL || bytes.IndexByte(fragment, 0) >= 0
		switch err {
		case nil:
			return containsNUL, bytesScanned, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return containsNUL, bytesScanned, err
		}
	}
}

func trimLineBreak(raw []byte, ended bool) []byte {
	if !ended || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return raw
	}
	raw = raw[:len(raw)-1]
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}
	return raw
}
