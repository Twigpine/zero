package agentsessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// headLimit bounds a discovery-time read of a transcript.
//
// Discovery lists sessions; it must never pay for their contents. A working
// machine here holds 439 MB of Claude Code transcripts across 1,266 files, one
// of them 73 MB on its own, so "just parse it and take the first few fields"
// turns `sessions discover` into something nobody runs twice.
//
// All three bounds are needed, and MaxBytes is the one that actually saves us:
// a single transcript line can be megabytes (one large tool result), so a
// line-count bound alone would still stream the whole file looking for the 64th
// newline. MaxBytes is enforced by an io.LimitReader around the file, which
// caps bytes pulled off disk regardless of where the newlines fall.
type headLimit struct {
	MaxLines     int
	MaxBytes     int64
	MaxLineBytes int
}

// defaultHeadLimit is sized from the real corpus, and MaxBytes in particular was
// set by measurement rather than by taste.
//
// Across sampled Claude Code transcripts the record carrying cwd/gitBranch/
// sessionId is line 3 and the ai-title record line 8, so 64 lines is ample. The
// byte budget is the subtle one: it is a budget for the whole scan, so a single
// outsized record spends it and starves the records after it. Three real
// sessions in a 269-file corpus open with a ~334 KB queue-operation record and
// were dropped entirely at a 256 KiB budget — the scan never reached line 3.
//
// 2 MiB clears that case with room to spare while still bounding a 73 MB
// transcript to a ~36x smaller read. The budget only ever binds on pathological
// files; a normal transcript's first 64 lines are a few KB in total and the line
// count ends the scan long before the bytes do.
// importLineLimit is the per-record cap for a FULL import, which is a
// deliberate one-off read of one file the user named — not the index's sweep of
// every transcript on disk. The discovery cap is 64 KiB because it is paid once
// per file across the whole store; applying it to an import silently deleted
// ordinary long messages from the conversation being restored. Still bounded, so
// a corrupt file cannot exhaust memory.
const importLineLimit = 8 << 20

var defaultHeadLimit = headLimit{
	MaxLines:     64,
	MaxBytes:     2 << 20,
	MaxLineBytes: 64 << 10,
}

// countingReader records how many bytes were actually pulled from the file, so
// tests can assert the bound holds rather than trusting that it does.
type countingReader struct {
	inner io.Reader
	count int64
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	read, err := reader.inner.Read(buffer)
	reader.count += int64(read)
	return read, err
}

// scanHeadSnapshot binds discovery metadata and its source identity to the same
// open file handle. Taking the snapshot in a second open leaves a replacement
// window where metadata can describe one transcript while Read is authorized
// to import another.
func scanHeadSnapshot(root string, path string, limit headLimit, visit func(line []byte, truncated bool) bool) (int64, sourceSnapshot, error) {
	file, err := openContained(root, path)
	if err != nil {
		return 0, sourceSnapshot{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return 0, sourceSnapshot{}, err
	}
	if !before.Mode().IsRegular() {
		return 0, sourceSnapshot{}, errors.New("agentsessions: transcript is not a regular file")
	}

	counter := &countingReader{inner: io.LimitReader(file, limit.MaxBytes)}
	reader := bufio.NewReaderSize(counter, 64<<10)

	for line := 0; line < limit.MaxLines; line++ {
		content, truncated, err := readBoundedLineTruncated(reader, limit.MaxLineBytes)
		if (len(content) > 0 || truncated) && !visit(content, truncated) {
			break
		}
		if err != nil {
			// EOF IS THE ONLY CLEAN STOP. Every other read error — a truncated
			// file, an I/O failure, a directory replaced mid-scan — was reported
			// as a successful partial scan, so a session indexed off whatever
			// bytes happened to arrive before the failure looked exactly like one
			// indexed off a whole file. The caller cannot decline what it is not
			// told about.
			if err == io.EOF {
				break
			}
			return counter.count, sourceSnapshot{}, err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return counter.count, sourceSnapshot{}, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return counter.count, sourceSnapshot{}, errors.New("agentsessions: transcript changed during discovery")
	}
	return counter.count, sourceSnapshot{info: after, size: after.Size(), modTime: after.ModTime()}, nil
}

// openContained opens path through a handle on root, so the containment checked
// when the path was globbed still holds at the moment of the read.
//
// THE GAP IS BETWEEN THE CHECK AND THE OPEN. globTranscripts already refuses a
// symlink wearing a transcript extension, but that verdict is about the state of
// the tree at glob time; anything can replace an entry before the file is
// actually opened, and os.Open would follow it out of the store. os.Root
// resolves every component itself and refuses to leave, so the window closes.
//
// An empty root opens directly, which is what the unit tests for this file need
// — they build a single transcript in a temp dir with no store around it.
func openContained(root string, path string) (*os.File, error) {
	if strings.TrimSpace(root) == "" {
		return os.Open(path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("transcript %s is outside the store root %s", path, root)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	return handle.Open(relative)
}

// streamTailLines visits complete records from at most the final maxBytes of a
// transcript. Foreign session files are append-only in practice, and resume
// needs their tail; bounding the window prevents one explicitly selected but
// enormous transcript from turning import into an unbounded disk/CPU job.
//
// prefixOmitted reports that older bytes were deliberately skipped. If the
// window begins in a record, that partial record is consumed and withheld so a
// JSON fragment can never masquerade as a complete event.
//
// It reads from an OPEN HANDLE rather than a path: the handle is the file whose
// identity was proved against the discovery snapshot (openSelectedSource), and
// reading by path again would be a second, unverified lookup. See
// validateSourceHandle for why that mattered.
func streamTailLines(file readSeekStater, maxLineBytes int, maxBytes int, visit func(line []byte, truncated bool) bool) (prefixOmitted bool, err error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	extent := info.Size()
	start := int64(0)
	if maxBytes > 0 && extent > int64(maxBytes) {
		prefixOmitted = true
		start = extent - int64(maxBytes)
		if _, err := file.Seek(start-1, io.SeekStart); err != nil {
			return false, err
		}
		previous := []byte{0}
		if _, err := io.ReadFull(file, previous); err != nil {
			return false, err
		}
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return false, err
		}
		if previous[0] != '\n' {
			reader := bufio.NewReaderSize(io.LimitReader(file, extent-start), 64<<10)
			if _, _, err := readBoundedLineTruncated(reader, 0); err != nil && err != io.EOF {
				return false, err
			}
			return prefixOmitted, streamReaderLines(reader, maxLineBytes, visit)
		}
	} else if _, err := file.Seek(start, io.SeekStart); err != nil {
		return false, err
	}

	return prefixOmitted, streamReaderLines(
		bufio.NewReaderSize(io.LimitReader(file, extent-start), 64<<10),
		maxLineBytes,
		visit,
	)
}

func streamReaderLines(reader *bufio.Reader, maxLineBytes int, visit func(line []byte, truncated bool) bool) error {
	for {
		content, truncated, err := readBoundedLineTruncated(reader, maxLineBytes)
		if (len(content) > 0 || truncated) && !visit(content, truncated) {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// readBoundedLineTruncated consumes through the next newline, returns at most
// keep bytes of it, and reports whether anything was discarded.
//
// bufio.Scanner is deliberately not used: it fails the whole scan on a token
// longer than its buffer, and these transcripts routinely contain lines far
// past any sensible buffer size. Here an overlong line is consumed and
// truncated, so one giant record costs a skip rather than the entire file.
//
// THE CALLER HAS TO BE ABLE TO TELL. A truncated record is returned as invalid
// JSON, and every caller reacted by skipping it — which is right for the index,
// where a session is still listed, and wrong for an import, where the skipped
// bytes were the conversation itself. Without this the two cases are
// indistinguishable, so the import path could not report what it had lost.
func readBoundedLineTruncated(reader *bufio.Reader, keep int) ([]byte, bool, error) {
	var kept []byte
	total := 0
	for {
		chunk, err := reader.ReadSlice('\n')
		total += len(chunk)
		if room := keep - len(kept); room > 0 {
			if room > len(chunk) {
				room = len(chunk)
			}
			// ReadSlice returns a view into the reader's buffer, invalidated by
			// the next read, so this must copy.
			kept = append(kept, chunk[:room]...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		// THE LINE TERMINATOR IS NOT CONTENT. Counting it made a record whose
		// content exactly fills keep report as truncated, and a CRLF file was one
		// byte worse — so an import emitted "could not be read" for records that
		// had in fact been read in full, which is a false alarm in the one place
		// this signal exists to be trusted.
		return bytes.TrimRight(kept, "\r\n"), total-terminatorBytes(chunk) > keep, err
	}
}

// terminatorBytes is the length of the trailing newline on a chunk, 0 when the
// final line of a file has none.
func terminatorBytes(chunk []byte) int {
	if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
		return 0
	}
	if len(chunk) > 1 && chunk[len(chunk)-2] == '\r' {
		return 2
	}
	return 1
}

// readSeekStater is what a bounded transcript read needs from its handle. It is
// satisfied by *os.File; naming it keeps the translators' contract explicit.
type readSeekStater interface {
	io.ReadSeeker
	Stat() (os.FileInfo, error)
}

// afterSourceOpen is a test seam: called with the transcript path once the
// import handle is open and verified, before any byte is read. A test uses it
// to replace the directory entry inside that exact interval. nil in production.
var afterSourceOpen func(path string)

// openSelectedSource opens the transcript discovery selected and proves, ON THE
// HANDLE THAT WILL BE READ, that it is still that file. The caller closes it.
//
// A PATHNAME IS A LOOKUP, NOT AN IDENTITY. Read used to validate the selected
// path through one handle, translate through a second open, and validate
// through a third. Each was a fresh lookup of the same name, so a concurrent
// writer could rename A aside, place a contained file B under A's name for the
// translation open, and restore A before the final check: both snapshot checks
// observed A and passed, while the bytes actually consumed came from B and
// were accepted under A's metadata and provenance. Not an escape from the
// store — B was inside the root — but the wrong file under the right name.
//
// One open, one identity. The discovery snapshot binds metadata to the inode
// discovery read; this binds the read to that same inode before the first byte
// and (validateSourceHandle) after the last one. The rooted open, regular-file
// check and bounded read are unchanged. Reported by @jatmn.
func openSelectedSource(root string, source ForeignSession) (*os.File, error) {
	if source.source.info == nil {
		return nil, errors.New("agentsessions: session source was not produced by discovery")
	}
	file, err := openContained(root, source.Path)
	if err != nil {
		return nil, fmt.Errorf("agentsessions: reopen selected session source: %w", err)
	}
	if err := validateSourceHandle(file, source); err != nil {
		_ = file.Close()
		return nil, err
	}
	if afterSourceOpen != nil {
		afterSourceOpen(source.Path)
	}
	return file, nil
}

// validateSourceHandle compares an OPEN handle with the discovery snapshot:
// same inode, same size, same modification time, still a regular file. Called
// before reading and again after, on the same handle, so continued writes
// during the read are refused as the existing changed-source failure rather
// than committed as a torn import.
func validateSourceHandle(file *os.File, source ForeignSession) error {
	if source.source.info == nil {
		return errors.New("agentsessions: session source was not produced by discovery")
	}
	current, err := file.Stat()
	if err != nil {
		return fmt.Errorf("agentsessions: stat selected session source: %w", err)
	}
	if !current.Mode().IsRegular() {
		return errors.New("agentsessions: transcript is not a regular file")
	}
	if !os.SameFile(source.source.info, current) ||
		source.source.size != current.Size() ||
		!source.source.modTime.Equal(current.ModTime()) {
		return errors.New("agentsessions: selected session source changed after discovery; discover it again before importing")
	}
	return nil
}

// topLevelStrings pulls named top-level string fields out of a JSON object that
// may be TRUNCATED, returning whatever appeared before the cut.
//
// WHY THIS EXISTS. A record longer than the per-line cap comes back as a prefix,
// fails json.Unmarshal, and is skipped whole. That is the right call for the
// record's content — the point of the cap is not to hold a giant tool result in
// memory — but the small metadata fields sit at the FRONT of the object, and
// throwing them away with the body cost the whole session: when the only
// cwd-bearing record was oversized, discovery had no workspace to bind to and
// dropped a transcript the user could see on disk.
//
// A token stream rather than a regex, because a regex over truncated JSON cannot
// tell a top-level "cwd" from one nested inside a message body or an escaped
// string that merely looks like a key. json.Decoder stops cleanly at the cut, so
// anything recovered here was genuinely complete and genuinely top-level, and
// anything after it is simply absent.
func topLevelStrings(prefix []byte, wanted ...string) map[string]string {
	found := map[string]string{}
	if len(wanted) == 0 {
		return found
	}
	want := map[string]bool{}
	for _, name := range wanted {
		want[name] = true
	}

	decoder := json.NewDecoder(bytes.NewReader(prefix))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return found
	}
	for len(found) < len(want) {
		keyToken, err := decoder.Token()
		if err != nil || keyToken == json.Delim('}') {
			return found
		}
		key, ok := keyToken.(string)
		if !ok {
			return found
		}
		if !want[key] {
			if skipValue(decoder) != nil {
				return found
			}
			continue
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return found
		}
		if value, ok := valueToken.(string); ok {
			found[key] = value
			continue
		}
		// A non-string value under a wanted key: step over whatever it opened.
		if delim, ok := valueToken.(json.Delim); ok && (delim == '{' || delim == '[') {
			if skipRest(decoder, 1) != nil {
				return found
			}
		}
	}
	return found
}

// skipValue consumes exactly one value, descending through nested objects and
// arrays so the next token read is the following key.
func skipValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok && (delim == '{' || delim == '[') {
		return skipRest(decoder, 1)
	}
	return nil
}

func skipRest(decoder *json.Decoder, depth int) error {
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
