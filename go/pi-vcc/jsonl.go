package pivcc

import (
	"bytes"
	"io"
	"os"
)

// Ports src/core/jsonl.ts: a chunked, streaming JSONL reader so giant sessions
// never materialize as one string.

// jsonlChunkSize is the read buffer size (64 KiB in TS).
const jsonlChunkSize = 64 * 1024

// ForEachJSONLLine calls onLine for every line of file, newline excluded, in
// file order. A final partial line is still delivered. It returns false when
// the file does not exist (pi does not create a new session's JSONL until its
// first persisted entry) and returns other IO errors as err.
//
// The TS version reads synchronously with readSync; this port streams with a
// bufio-free chunked reader that keeps the same line-splitting contract.
func ForEachJSONLLine(file string, onLine func(line []byte)) (found bool, err error) {
	handle, openErr := os.Open(file)
	if openErr != nil {
		if os.IsNotExist(openErr) {
			return false, nil
		}
		return false, openErr
	}
	defer handle.Close()

	buf := make([]byte, jsonlChunkSize)
	var pending []byte
	for {
		n, readErr := handle.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for {
				idx := bytes.IndexByte(chunk, '\n')
				if idx < 0 {
					break
				}
				segment := chunk[:idx]
				if len(pending) > 0 {
					pending = append(pending, segment...)
					onLine(pending)
					pending = nil
				} else {
					onLine(segment)
				}
				chunk = chunk[idx+1:]
			}
			if len(chunk) > 0 {
				pending = append(pending, chunk...)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return true, readErr
		}
	}
	if len(pending) > 0 {
		onLine(pending)
	}
	return true, nil
}
