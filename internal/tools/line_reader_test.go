package tools

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadRawLineLimitedCRLFAtLimit(t *testing.T) {
	for _, test := range []struct {
		name    string
		maxKeep int
		want    string
		clipped bool
	}{
		{name: "content plus CRLF", maxKeep: 10, want: "abcdefghij", clipped: false},
		{name: "content plus CRLF over limit", maxKeep: 11, want: "abcdefghij", clipped: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			line, ended, clipped, containsNUL, _, err := readRawLineLimited(bufio.NewReader(strings.NewReader("abcdefghij\r\n")), test.maxKeep)
			if err != nil {
				t.Fatal(err)
			}
			if string(line) != test.want || !ended || clipped != test.clipped || containsNUL {
				t.Fatalf("line=%q ended=%v clipped=%v containsNUL=%v", line, ended, clipped, containsNUL)
			}
		})
	}
}

func TestReadRawLineLimitedPropagatesFullLineError(t *testing.T) {
	wantErr := errors.New("full line failed")
	source := &sequenceReader{steps: []readStep{
		{data: []byte(strings.Repeat("x", 16)), err: bufio.ErrBufferFull},
		{data: []byte("tail"), err: wantErr},
	}}
	reader := bufio.NewReader(source)
	_, _, _, _, _, err := readRawLineLimited(reader, 16)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
	if source.reads != 2 {
		t.Fatalf("reads=%d want 2", source.reads)
	}
}

func TestReadRawLineLimitedPropagatesOverflowError(t *testing.T) {
	wantErr := errors.New("read failed")
	source := &sequenceReader{steps: []readStep{{data: []byte(strings.Repeat("x", 17)), err: wantErr}}}
	reader := bufio.NewReader(source)
	_, _, _, _, _, err := readRawLineLimited(reader, 16)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v want %v", err, wantErr)
	}
	if source.reads != 1 {
		t.Fatalf("reads=%d want 1", source.reads)
	}
}

type readStep struct {
	data []byte
	err  error
}

type sequenceReader struct {
	steps []readStep
	reads int
}

func (reader *sequenceReader) Read(buffer []byte) (int, error) {
	reader.reads++
	if len(reader.steps) == 0 {
		return 0, io.EOF
	}
	step := reader.steps[0]
	n := copy(buffer, step.data)
	if n < len(step.data) {
		reader.steps[0].data = step.data[n:]
	} else {
		reader.steps = reader.steps[1:]
	}
	return n, step.err
}
