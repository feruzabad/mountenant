package contenttype

import "testing"

func TestForPath(t *testing.T) {
	for p, want := range map[string]string{
		"disc/debian netinst.iso": "application/x-iso9660-image",
		"Movie.2026.MKV":          "video/x-matroska",
		"notes.nfo":               "text/plain; charset=utf-8",
		"archive.part01.rar":      "application/vnd.rar",
		"file.par2":               Default,
		"noext":                   Default,
		"dir.iso/file":            Default,
	} {
		if got := ForPath(p); got != want {
			t.Errorf("%q: %q, want %q", p, got, want)
		}
	}
}
