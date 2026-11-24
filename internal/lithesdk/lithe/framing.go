package lithe

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	maxHeaderBytes  = 8192
	maxMessageBytes = 64 << 20
)

// framingError ends the session: the stream can no longer be read in step.
type framingError struct{ message string }

func (e *framingError) Error() string { return e.message }

// frame is one message read from the host. A body that is not JSON is
// malformed; the stream stays in step and the session goes on.
type frame struct {
	value     any
	malformed bool
}

// readFrame reads one Content-Length framed message. It returns nil, nil
// when the input ends between messages.
func readFrame(reader *bufio.Reader) (*frame, error) {
	var (
		total    int
		length   string
		found    bool
		started  bool
		line     []byte
		finished bool
	)
	for !finished {
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !started {
					return nil, nil
				}
				return nil, &framingError{"The input ended inside a message header."}
			}
			return nil, &framingError{"The input failed: " + err.Error()}
		}
		started = true
		total++
		if total > maxHeaderBytes {
			return nil, &framingError{"The message header is too long."}
		}
		if b != '\n' {
			line = append(line, b)
			continue
		}
		text := strings.TrimSuffix(string(line), "\r")
		line = line[:0]
		if text == "" {
			finished = true
			continue
		}
		name, value, ok := strings.Cut(text, ":")
		if ok && !found && strings.EqualFold(strings.TrimSpace(name), "content-length") {
			length, found = strings.TrimSpace(value), true
		}
	}
	size, valid := contentLength(length, found)
	if !valid {
		return nil, &framingError{"A message has no valid Content-Length header."}
	}
	if size > maxMessageBytes {
		return nil, &framingError{fmt.Sprintf("A message of %d bytes exceeds the %d-byte limit.", size, maxMessageBytes)}
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, &framingError{"The input failed: " + err.Error()}
	}
	value, err := decodeJSON(body)
	if err != nil {
		return &frame{malformed: true}, nil
	}
	return &frame{value: value}, nil
}

func contentLength(value string, found bool) (uint64, bool) {
	if !found || value == "" {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	size, err := strconv.ParseUint(value, 10, 64)
	return size, err == nil
}

// writeFrame writes one framed message and flushes it.
func writeFrame(writer *bufio.Writer, body []byte) error {
	if _, err := fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	if _, err := writer.Write(body); err != nil {
		return err
	}
	return writer.Flush()
}
