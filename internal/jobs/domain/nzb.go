package domain

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NZB intake errors (spec §6.5).
var (
	ErrInvalidNZB  = errors.New("invalid NZB")
	ErrNZBTooLarge = errors.New("NZB too large")
)

// NZB is a validated upload, normalised to plain XML (ADR 0005).
type NZB struct {
	XML      []byte   // exactly what the backend receives and nzb_blobs stores
	Digest   [32]byte // SHA-256 of XML; the same NZB plain or gzipped has one digest
	Files    int
	Segments int
	Bytes    int64 // sum of the segments' declared sizes
}

var gzipMagic = []byte{0x1f, 0x8b}

// ReadNZB reads an uploaded .nzb or .nzb.gz. Gzip is recognised by its magic
// bytes, not by the file name. limit bounds the decompressed XML, so a gzip
// bomb costs at most limit bytes of memory.
func ReadNZB(r io.Reader, limit int64) (NZB, error) {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if head, _ := br.Peek(2); bytes.Equal(head, gzipMagic) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return NZB{}, fmt.Errorf("%w: broken gzip stream", ErrInvalidNZB)
		}
		defer zr.Close()
		src = zr
	}
	xmlBytes, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		if errors.Is(err, gzip.ErrChecksum) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) {
			return NZB{}, fmt.Errorf("%w: broken gzip stream", ErrInvalidNZB)
		}
		return NZB{}, err
	}
	if int64(len(xmlBytes)) > limit {
		return NZB{}, fmt.Errorf("%w: more than %d bytes of XML", ErrNZBTooLarge, limit)
	}
	n, err := validateNZB(xmlBytes)
	if err != nil {
		return NZB{}, err
	}
	n.XML = xmlBytes
	n.Digest = sha256.Sum256(xmlBytes)
	return n, nil
}

// validateNZB parses the document with encoding/xml, which never resolves
// external entities, and enforces the DOCTYPE rules of ADR 0005.
func validateNZB(b []byte) (NZB, error) {
	invalid := func(format string, a ...any) (NZB, error) {
		return NZB{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalidNZB}, a...)...)
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = true
	dec.CharsetReader = charsetReader

	var n NZB
	var stack []string
	sawRoot, sawDoctype := false, false
	fileSegments := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return invalid("%v", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			d := string(t)
			if !strings.HasPrefix(d, "DOCTYPE") || sawDoctype || sawRoot {
				return invalid("unexpected <!%s>", truncate(d, 40))
			}
			// The standard <!DOCTYPE nzb PUBLIC "…" "…"> is fine; an internal
			// subset or entity declaration is how XML attacks start.
			if strings.ContainsAny(d, "[]") || strings.Contains(strings.ToUpper(d), "ENTITY") {
				return invalid("DOCTYPE with an internal subset or entity declarations")
			}
			sawDoctype = true
		case xml.StartElement:
			name := t.Name.Local
			switch {
			case len(stack) == 0:
				if name != "nzb" || sawRoot {
					return invalid("root element must be <nzb>")
				}
				sawRoot = true
			case name == "file" && len(stack) == 1:
				n.Files++
			case name == "segment" && len(stack) == 3 && stack[1] == "file" && stack[2] == "segments":
				n.Segments++
				fileSegments++
				for _, a := range t.Attr {
					if a.Name.Local == "bytes" {
						if v, err := strconv.ParseInt(a.Value, 10, 64); err == nil && v > 0 {
							n.Bytes += v
						}
					}
				}
			}
			stack = append(stack, name)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if !sawRoot {
		return invalid("no <nzb> element")
	}
	if n.Files == 0 || n.Segments == 0 {
		return invalid("no files or no segments")
	}
	return n, nil
}

// charsetReader supports the encodings real NZBs declare besides UTF-8.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		return latin1Reader{bufio.NewReader(in)}, nil
	}
	return nil, fmt.Errorf("unsupported encoding %q", label)
}

// latin1Reader converts ISO-8859-1 to UTF-8: every byte is one code point.
type latin1Reader struct{ r io.ByteReader }

func (l latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n+utf8.UTFMax <= len(p) {
		c, err := l.r.ReadByte()
		if err != nil {
			if n > 0 && errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
		n += utf8.EncodeRune(p[n:], rune(c))
	}
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// maxNameRunes bounds displayed NZB names.
const maxNameRunes = 200

// SanitizeNZBName turns an uploaded file name into a display name: base name
// only, no control characters, bounded length. It is never used as a path
// (spec §7.6).
func SanitizeNZBName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	var b strings.Builder
	runes := 0
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if runes == maxNameRunes {
			break
		}
		b.WriteRune(r)
		runes++
	}
	s := strings.TrimSpace(b.String())
	if s == "" || s == "." || s == "/" || s == ".." {
		return "upload.nzb"
	}
	return s
}
