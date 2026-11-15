package fsops

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Kind says what Read returned.
type Kind string

const (
	KindText     Kind = "text"
	KindImage    Kind = "image"
	KindBinary   Kind = "binary"
	KindNotebook Kind = "notebook"
)

const (
	DefaultReadLines = 2000
	lineClip         = 2000    // bytes kept of one line
	maxReadOutput    = 1 << 20 // bytes of numbered text returned by one read
	sniffLen         = 8192
	emptyWarning     = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
)

// ReadOptions select a line range. Offset is the 1-based first line (0 means 1); Limit is the
// number of lines (0 means DefaultReadLines).
type ReadOptions struct {
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

// Image is base64 image data with its MIME type.
type Image struct {
	MIME string `json:"mime"`
	Data string `json:"data"`
}

// ReadResult is what Read returns. Text holds numbered lines ("%6d<TAB>line") for text files,
// the rendered cells for notebooks, or a one-line description for binary files.
type ReadResult struct {
	Path       string  `json:"path"`
	Kind       Kind    `json:"kind"`
	Size       int64   `json:"size"`
	Text       string  `json:"text,omitempty"`
	StartLine  int     `json:"start_line,omitempty"`
	EndLine    int     `json:"end_line,omitempty"`
	TotalLines int     `json:"total_lines,omitempty"` // set when the end of the file was reached
	More       bool    `json:"more,omitempty"`        // the range stopped before the end of the file
	NextOffset int     `json:"next_offset,omitempty"` // offset that continues a range cut short
	Images     []Image `json:"images,omitempty"`
	MIMEType   string  `json:"mime_type,omitempty"` // sniffed type of a binary file
	SHA256     string  `json:"sha256,omitempty"`    // of a binary file
}

var imageMIMEs = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
}

// Read returns a text file by line range with line numbers, an image as base64, a notebook
// rendered cell by cell, or for any other binary file its size, type and SHA-256.
func (f *FS) Read(path string, opt ReadOptions) (*ReadResult, error) {
	if opt.Offset < 0 || opt.Limit < 0 {
		return nil, fail(ErrInvalid, "offset and limit must not be negative")
	}
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	file, fi, err := f.openRegular(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	res := &ReadResult{Path: rel, Size: fi.Size()}
	ext := strings.ToLower(filepath.Ext(rel))
	br := bufio.NewReaderSize(file, 64<<10)
	sniff, _ := br.Peek(sniffLen)

	if want, ok := imageMIMEs[ext]; ok && http.DetectContentType(sniff) == want {
		if fi.Size() > f.opt.MaxImage {
			return nil, fail(ErrTooLarge, "Image file is too large (%d bytes, max %d bytes)", fi.Size(), f.opt.MaxImage)
		}
		data, err := io.ReadAll(io.LimitReader(br, f.opt.MaxImage+1))
		if err != nil {
			return nil, mapErr(rel, err)
		}
		res.Kind = KindImage
		res.Images = []Image{{MIME: want, Data: base64.StdEncoding.EncodeToString(data)}}
		return res, nil
	}
	if ext == ".ipynb" {
		if fi.Size() > f.opt.MaxFile {
			return nil, fail(ErrTooLarge, "File is too large (%d bytes, max %d bytes)", fi.Size(), f.opt.MaxFile)
		}
		data, err := io.ReadAll(io.LimitReader(br, f.opt.MaxFile+1))
		if err != nil {
			return nil, mapErr(rel, err)
		}
		nb, err := parseNotebook(data)
		if err != nil {
			return nil, err
		}
		res.Kind = KindNotebook
		res.Text, res.Images = nb.render()
		return res, nil
	}
	if isBinary(sniff) {
		h := sha256.New()
		if _, err := io.Copy(h, br); err != nil {
			return nil, mapErr(rel, err)
		}
		res.Kind = KindBinary
		res.MIMEType = http.DetectContentType(sniff)
		res.SHA256 = hex.EncodeToString(h.Sum(nil))
		res.Text = fmt.Sprintf("Binary file (%s, %d bytes, sha256 %s); its content is not shown.", res.MIMEType, res.Size, res.SHA256)
		return res, nil
	}
	return readText(br, res, opt)
}

func readText(br *bufio.Reader, res *ReadResult, opt ReadOptions) (*ReadResult, error) {
	off, limit := max(opt.Offset, 1), opt.Limit
	if limit == 0 {
		limit = DefaultReadLines
	}
	res.Kind = KindText
	var sb strings.Builder
	n := 0
	for {
		line, clipped, ok, err := readLine(br)
		if err != nil {
			return nil, fail(ErrInvalid, "read: %v", err)
		}
		if !ok {
			res.TotalLines = n
			break
		}
		n++
		if n < off {
			continue
		}
		if n >= off+limit || sb.Len() >= maxReadOutput {
			res.More, res.NextOffset = true, n
			break
		}
		if res.StartLine == 0 {
			res.StartLine = n
		}
		res.EndLine = n
		s := strings.ToValidUTF8(string(line), "\xef\xbf\xbd")
		if clipped {
			s += " ... [line truncated]"
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", n, s)
	}
	switch {
	case res.TotalLines == 0 && !res.More:
		res.Text = emptyWarning
	case res.StartLine == 0 && !res.More:
		res.Text = fmt.Sprintf("<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>", off, res.TotalLines)
	default:
		res.Text = sb.String()
	}
	return res, nil
}

// readLine reads one line without its line ending. A line longer than lineClip is cut (clipped).
// ok is false at the end of the file.
func readLine(br *bufio.Reader) (line []byte, clipped, ok bool, err error) {
	var buf []byte
	for {
		part, e := br.ReadSlice('\n')
		if len(part) > 0 {
			ok = true
		}
		if room := lineClip + 2 - len(buf); room > 0 {
			buf = append(buf, part[:min(room, len(part))]...)
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if e != nil && e != io.EOF {
			return nil, false, false, e
		}
		break
	}
	if !ok {
		return nil, false, false, nil
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	buf = bytes.TrimSuffix(buf, []byte("\r"))
	if len(buf) > lineClip {
		cut := lineClip
		for cut > 0 && !utf8.RuneStart(buf[cut]) {
			cut--
		}
		buf, clipped = buf[:cut], true
	}
	return buf, clipped, true, nil
}

// isBinary reports a NUL byte or invalid UTF-8 in the first bytes of a file.
func isBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	if len(b) == sniffLen { // the cut may split a rune
		for i := 1; i <= 3 && i <= len(b); i++ {
			if utf8.RuneStart(b[len(b)-i]) {
				if !utf8.FullRune(b[len(b)-i:]) {
					b = b[:len(b)-i]
				}
				break
			}
		}
	}
	return !utf8.Valid(b)
}
