package evidence

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"strings"
)

var authorization = regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]*`)
var assignment = regexp.MustCompile(`(?i)(["']?(?:[a-z0-9_]*(?:token|password|passwd|secret|api[_-]?key|access[_-]?key))["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&]+)`)
var urlCredential = regexp.MustCompile(`(://)[^\s/@:]+:[^\s/@]+@`)

func redact(s string) string {
	s = authorization.ReplaceAllString(s, "${1}[REDACTED]")
	s = assignment.ReplaceAllString(s, "${1}[REDACTED]")
	return urlCredential.ReplaceAllString(s, "${1}[REDACTED]@")
}

// frameReader decodes without allocating according to an untrusted frame length.
type frameReader struct {
	reader    io.Reader
	remaining uint32
}

func (r *frameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.remaining == 0 {
		var header [8]byte
		n, err := io.ReadFull(r.reader, header[:])
		if err != nil {
			if err == io.EOF && n == 0 {
				return 0, io.EOF
			}
			return 0, errors.New("incomplete Docker log frame")
		}
		if (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return 0, errors.New("invalid Docker log frame")
		}
		r.remaining = binary.BigEndian.Uint32(header[4:])
	}
	if uint32(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= uint32(n)
	if err == io.EOF && r.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// Reads no more than allowance wire bytes, including frame headers. Incomplete
// and oversized lines are discarded before redaction, never copied to disk.
func readLogs(raw io.Reader, tty bool, allowance int64) (string, bool, error) {
	limited := &io.LimitedReader{R: raw, N: allowance}
	var decoded io.Reader = limited
	if !tty {
		decoded = &frameReader{reader: limited}
	}
	reader := bufio.NewReaderSize(decoded, 4096)
	var out strings.Builder
	truncated, dropping := false, false
	for {
		fragment, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			truncated, dropping = true, true
			continue
		}
		if dropping {
			dropping = false
		} else if len(fragment) > 0 {
			if err != nil {
				truncated = true
			} else {
				clean := redact(strings.ToValidUTF8(string(fragment), "�"))
				if int64(out.Len()+len(clean)) > allowance {
					truncated = true
				} else {
					out.WriteString(clean)
				}
			}
		}
		if err != nil {
			if limited.N == 0 {
				truncated = true
			}
			if err == io.EOF {
				return out.String(), truncated, nil
			}
			return out.String(), truncated, err
		}
	}
}
