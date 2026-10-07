package domain

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

const doctype = `<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">`

func nzbDoc(prolog, body string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` + "\n" + prolog + "\n" +
		`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + body + `</nzb>`
}

const oneFile = `<head><meta type="name">debian</meta></head>
<file poster="p" date="1" subject="debian.iso (1/2)">
  <groups><group>alt.binaries.test</group></groups>
  <segments>
    <segment bytes="700000" number="1">a@b</segment>
    <segment bytes="300000" number="2">c@d</segment>
  </segments>
</file>`

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestReadNZBPlainAndGzip(t *testing.T) {
	doc := nzbDoc(doctype, oneFile)
	plain, err := ReadNZB(strings.NewReader(doc), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Files != 1 || plain.Segments != 2 || plain.Bytes != 1_000_000 {
		t.Fatalf("%+v", plain)
	}
	if plain.Digest != sha256.Sum256([]byte(doc)) {
		t.Fatal("digest is not over the XML")
	}
	zipped, err := ReadNZB(bytes.NewReader(gz(doc)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if zipped.Digest != plain.Digest || !bytes.Equal(zipped.XML, plain.XML) {
		t.Fatal("gzipped upload differs from plain")
	}
}

func TestReadNZBRejects(t *testing.T) {
	cases := map[string]string{
		"internal subset": nzbDoc(`<!DOCTYPE nzb [<!ENTITY x "y">]>`, oneFile),
		"xxe":             nzbDoc(`<!DOCTYPE nzb SYSTEM "file:///etc/passwd" [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>`, oneFile),
		"entity word":     nzbDoc(`<!DOCTYPE nzb PUBLIC "ENTITY" "x">`, oneFile),
		"two doctypes":    nzbDoc(doctype+doctype, oneFile),
		"undefined ent":   nzbDoc(doctype, strings.Replace(oneFile, "debian", "&lol;", 1)),
		"no files":        nzbDoc(doctype, `<head></head>`),
		"no segments":     nzbDoc(doctype, `<file subject="x"><groups/><segments></segments></file>`),
		"wrong root":      `<?xml version="1.0"?><html><file/></html>`,
		"not xml":         "hello",
		"truncated":       nzbDoc(doctype, oneFile)[:200],
		"bad encoding":    strings.Replace(nzbDoc(doctype, oneFile), "utf-8", "shift_jis", 1),
		"empty":           "",
	}
	for name, doc := range cases {
		if _, err := ReadNZB(strings.NewReader(doc), 1<<20); !errors.Is(err, ErrInvalidNZB) {
			t.Errorf("%s: %v", name, err)
		}
	}
	broken := gz(nzbDoc(doctype, oneFile))
	broken[len(broken)-5] ^= 0xff // corrupt the CRC
	if _, err := ReadNZB(bytes.NewReader(broken), 1<<20); !errors.Is(err, ErrInvalidNZB) {
		t.Errorf("broken gzip: %v", err)
	}
}

func TestReadNZBLatin1(t *testing.T) {
	doc := strings.Replace(nzbDoc(doctype, oneFile), "utf-8", "iso-8859-1", 1)
	doc = strings.Replace(doc, "debian", "d\xe9bian", 1) // é in Latin-1
	n, err := ReadNZB(strings.NewReader(doc), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(n.XML, []byte("d\xe9bian")) {
		t.Fatal("XML must be passed on unchanged")
	}
}

func TestReadNZBSizeLimitAndBomb(t *testing.T) {
	doc := nzbDoc(doctype, oneFile)
	if _, err := ReadNZB(strings.NewReader(doc), int64(len(doc))); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if _, err := ReadNZB(strings.NewReader(doc), int64(len(doc)-1)); !errors.Is(err, ErrNZBTooLarge) {
		t.Fatalf("one byte over: %v", err)
	}
	// 64 MiB of zeros compress to ~64 KiB; the limit applies to the output.
	bomb := gz(strings.Repeat("\x00", 64<<20))
	if _, err := ReadNZB(bytes.NewReader(bomb), 1<<20); !errors.Is(err, ErrNZBTooLarge) {
		t.Fatalf("gzip bomb: %v", err)
	}
}

func TestSanitizeNZBName(t *testing.T) {
	for in, want := range map[string]string{
		"Debian.10.nzb":           "Debian.10.nzb",
		"../../etc/passwd":        "passwd",
		`C:\Users\x\movie.nzb.gz`: "movie.nzb.gz",
		"bad\x00\nname\u2028.nzb": "badname.nzb",
		"":                        "upload.nzb",
		"..":                      "upload.nzb",
		"   ":                     "upload.nzb",
	} {
		if got := SanitizeNZBName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := SanitizeNZBName(strings.Repeat("ä", 500)); len([]rune(got)) != maxNameRunes {
		t.Errorf("length %d", len([]rune(got)))
	}
}
