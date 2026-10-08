package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxMessageBytes caps a single framed MCP message so a hostile or buggy peer
// cannot drive an unbounded allocation with an enormous Content-Length header
// (Atoi accepts values up to ~9.2e18). 64 MiB is far above any legitimate
// JSON-RPC payload Zero exchanges.
const maxMessageBytes = 64 * 1024 * 1024

type rpcMessage struct {
	JSONRPC       string          `json:"jsonrpc,omitempty"`
	ID            any             `json:"id,omitempty"`
	Method        string          `json:"method,omitempty"`
	Params        json.RawMessage `json:"params,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         *rpcError       `json:"error,omitempty"`
	methodPresent bool            `json:"-"`
}

func (m rpcMessage) isRequestOrNotification() bool {
	return m.methodPresent || m.Method != ""
}

func (m *rpcMessage) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var wire struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	m.JSONRPC = wire.JSONRPC
	m.ID = wire.ID
	m.Params = wire.Params
	m.Result = wire.Result
	m.Error = wire.Error
	if raw, ok := probe["method"]; ok {
		m.methodPresent = true
		var method string
		if err := json.Unmarshal(raw, &method); err == nil {
			m.Method = method
		}
	}
	return nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type messageReader struct {
	reader *bufio.Reader
}

type messageWriter struct {
	writer *bufio.Writer
}

func newMessageReader(reader io.Reader) *messageReader {
	return &messageReader{reader: bufio.NewReader(reader)}
}

func newMessageWriter(writer io.Writer) *messageWriter {
	return &messageWriter{writer: bufio.NewWriter(writer)}
}

// read returns the next JSON-RPC message. The MCP stdio transport frames each
// message as newline-delimited JSON (one message per line, no embedded
// newlines); for backward compatibility it also accepts LSP-style
// `Content-Length` framing. Both paths are bounded by maxMessageBytes so a
// hostile peer cannot exhaust memory.
func (reader *messageReader) read() (rpcMessage, error) {
	for {
		line, err := reader.readLine()
		if err != nil {
			// A final message without a trailing newline still arrives with EOF.
			if errors.Is(err, io.EOF) {
				if trimmed := strings.TrimSpace(line); isJSONStart(trimmed) {
					return decodeMessage([]byte(trimmed))
				}
			}
			return rpcMessage{}, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue // skip blank separator lines between messages
		}
		if isJSONStart(trimmed) {
			return decodeMessage([]byte(trimmed))
		}
		// Not JSON — treat this line as the start of an LSP-style header block.
		return reader.readHeaderFramed(strings.TrimRight(line, "\r\n"))
	}
}

func isJSONStart(s string) bool {
	return s != "" && (s[0] == '{' || s[0] == '[')
}

func decodeMessage(body []byte) (rpcMessage, error) {
	var message rpcMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return rpcMessage{}, fmt.Errorf("invalid MCP JSON-RPC message: %w", err)
	}
	return message, nil
}

// readLine reads a single line (without the trailing newline), bounded to
// maxMessageBytes so an unterminated stream cannot drive unbounded allocation.
func (reader *messageReader) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := reader.reader.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxMessageBytes {
			return "", fmt.Errorf("MCP message exceeds %d-byte limit", maxMessageBytes)
		}
		if err == nil {
			return string(buf), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(buf), err
	}
}

// readHeaderFramed parses an LSP-style header block (starting with firstLine)
// up to the blank separator, then reads the Content-Length body.
func (reader *messageReader) readHeaderFramed(firstLine string) (rpcMessage, error) {
	contentLength := 0
	line := firstLine
	for {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "content-length") {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed <= 0 {
				return rpcMessage{}, fmt.Errorf("invalid MCP content length %q", value)
			}
			if parsed > maxMessageBytes {
				return rpcMessage{}, fmt.Errorf("MCP content length %d exceeds %d-byte limit", parsed, maxMessageBytes)
			}
			contentLength = parsed
		}
		next, err := reader.readLine()
		if err != nil {
			return rpcMessage{}, err
		}
		if strings.TrimRight(next, "\r\n") == "" {
			break
		}
		line = strings.TrimRight(next, "\r\n")
	}
	if contentLength <= 0 {
		return rpcMessage{}, fmt.Errorf("missing MCP content length")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader.reader, body); err != nil {
		return rpcMessage{}, err
	}
	return decodeMessage(body)
}

// write emits a message using MCP stdio newline-delimited JSON framing. json
// output contains no literal newlines, so the single trailing '\n' is the only
// delimiter.
func (writer *messageWriter) write(message rpcMessage) error {
	if message.JSONRPC == "" {
		message.JSONRPC = "2.0"
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := writer.writer.Write(body); err != nil {
		return err
	}
	if err := writer.writer.WriteByte('\n'); err != nil {
		return err
	}
	return writer.writer.Flush()
}
