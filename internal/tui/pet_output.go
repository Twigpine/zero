package tui

import (
	"bytes"
	"io"
	"sync"

	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/terminalpet"
)

const (
	terminalSyncStart = "\x1b[?2026h"
	terminalSyncEnd   = "\x1b[?2026l"
)

type terminalOutputFile interface {
	io.ReadWriteCloser
	Fd() uintptr
}

type petImageOutput struct {
	output         terminalOutputFile
	renderers      []*terminalpet.ImageRenderer
	renderDisabled []bool
	mu             sync.Mutex
}

func newPetImageOutput(output terminalOutputFile, renderers ...*terminalpet.ImageRenderer) *petImageOutput {
	active := make([]*terminalpet.ImageRenderer, 0, len(renderers))
	for _, renderer := range renderers {
		if renderer != nil {
			active = append(active, renderer)
		}
	}
	return &petImageOutput{output: output, renderers: active, renderDisabled: make([]bool, len(active))}
}

func (o *petImageOutput) Read(value []byte) (int, error) {
	return o.output.Read(value)
}

func (o *petImageOutput) Write(value []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	leavingAltScreen := bytes.Contains(value, []byte("\x1b[?1049l"))
	if leavingAltScreen {
		for _, renderer := range o.renderers {
			if !rendererUsesKitty(renderer) {
				if err := o.writeImageUpdate(renderer.Clear); err != nil {
					return 0, err
				}
			}
		}
	}
	if leavingAltScreen {
		written, err := o.output.Write(value)
		if err != nil {
			return written, err
		}
		for _, renderer := range o.renderers {
			if rendererUsesKitty(renderer) {
				if err := o.writeImageUpdate(renderer.Clear); err != nil {
					return written, err
				}
			}
		}
		return written, nil
	}
	if bytes.Contains(value, []byte(ansi.EraseEntireScreen)) ||
		bytes.Contains(value, []byte(ansi.SetModeAltScreenSaveCursor)) {
		for _, renderer := range o.renderers {
			renderer.Invalidate()
		}
	}

	var imageUpdate bytes.Buffer
	for index, renderer := range o.renderers {
		if o.renderDisabled[index] {
			continue
		}
		var rendererUpdate bytes.Buffer
		if err := renderer.Render(&rendererUpdate); err != nil {
			// Terminal images are decorative. Disable only the failing renderer for
			// this output session so an attachment issue cannot hide a companion
			// (or vice versa) or terminate the interactive shell.
			o.renderDisabled[index] = true
			continue
		}
		imageUpdate.Write(rendererUpdate.Bytes())
	}
	if imageUpdate.Len() == 0 {
		return writeChecked(o.output, value)
	}

	// Bubble Tea encloses supported terminal frames in synchronized-output
	// markers. Keep the pet placement inside that same transaction so the
	// terminal never presents the text frame and image movement separately.
	syncStart := bytes.LastIndex(value, []byte(terminalSyncStart))
	syncEnd := bytes.LastIndex(value, []byte(terminalSyncEnd))
	if syncEnd >= 0 && syncEnd > syncStart {
		var frame bytes.Buffer
		frame.Grow(len(value) + imageUpdate.Len())
		frame.Write(value[:syncEnd])
		frame.Write(imageUpdate.Bytes())
		frame.Write(value[syncEnd:])
		written, err := o.output.Write(frame.Bytes())
		consumed := originalBytesWritten(written, syncEnd, imageUpdate.Len(), len(value))
		if err == nil && written != frame.Len() {
			err = io.ErrShortWrite
		}
		if err != nil {
			return consumed, err
		}
		return len(value), nil
	}
	if syncStart > syncEnd {
		written, writeErr := writeChecked(o.output, value)
		if writeErr != nil {
			return written, writeErr
		}
		_, imageErr := writeChecked(o.output, imageUpdate.Bytes())
		return written, imageErr
	}

	if _, err := io.WriteString(o.output, terminalSyncStart); err != nil {
		return 0, err
	}
	written, writeErr := writeChecked(o.output, value)
	if writeErr == nil {
		_, writeErr = writeChecked(o.output, imageUpdate.Bytes())
	}
	_, endErr := io.WriteString(o.output, terminalSyncEnd)
	if writeErr != nil {
		return written, writeErr
	}
	return written, endErr
}

func rendererUsesKitty(renderer *terminalpet.ImageRenderer) bool {
	switch renderer.Support().Protocol {
	case terminalpet.ImageProtocolKitty, terminalpet.ImageProtocolKittyLocalFile:
		return true
	default:
		return false
	}
}

func writeChecked(writer io.Writer, value []byte) (int, error) {
	written, err := writer.Write(value)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	return written, err
}

// originalBytesWritten maps progress through an expanded synchronized frame
// back to bytes consumed from the caller's original buffer. Bytes belonging
// to the injected image update must never be reported as caller bytes.
func originalBytesWritten(written, prefixLength, injectedLength, originalLength int) int {
	if written <= 0 {
		return 0
	}
	if written <= prefixLength {
		return written
	}
	consumed := prefixLength
	suffixWritten := written - prefixLength - injectedLength
	if suffixWritten > 0 {
		consumed += min(suffixWritten, originalLength-prefixLength)
	}
	return min(consumed, originalLength)
}

func (o *petImageOutput) writeImageUpdate(render func(io.Writer) error) error {
	var update bytes.Buffer
	if err := render(&update); err != nil {
		return err
	}
	if update.Len() == 0 {
		return nil
	}
	if _, err := io.WriteString(o.output, terminalSyncStart); err != nil {
		return err
	}
	_, writeErr := o.output.Write(update.Bytes())
	_, endErr := io.WriteString(o.output, terminalSyncEnd)
	if writeErr != nil {
		return writeErr
	}
	return endErr
}

func (o *petImageOutput) Close() error {
	return nil
}

func (o *petImageOutput) Fd() uintptr {
	return o.output.Fd()
}

func (o *petImageOutput) clearImage() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.writeImageUpdate(func(writer io.Writer) error {
		if _, err := io.WriteString(writer, ansi.ResetModeMouseExtSgrPixel); err != nil {
			return err
		}
		for _, renderer := range o.renderers {
			if err := renderer.Clear(writer); err != nil {
				return err
			}
		}
		if len(o.renderers) == 0 {
			return nil
		}
		ids := append([]uint32{petAmbientImageID, petPreviewImageID}, attachmentPreviewImageIDs()...)
		return o.renderers[0].DeleteImages(writer, ids...)
	})
}
