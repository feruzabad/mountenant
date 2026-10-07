// Package contenttype maps file extensions to media types (spec §10.6).
//
// The table is fixed on purpose: mime.TypeByExtension merges the host's
// mime.types files, so the same file got application/x-iso9660-image on one
// machine and application/vnd.efi.iso on another. Responses must not depend
// on the server's OS packages.
package contenttype

import (
	"path"
	"strings"
)

// Default is used for every extension not in the table.
const Default = "application/octet-stream"

var byExt = map[string]string{
	// disc images and archives
	".iso": "application/x-iso9660-image",
	".img": Default,
	".zip": "application/zip",
	".rar": "application/vnd.rar",
	".7z":  "application/x-7z-compressed",
	".gz":  "application/gzip",
	".tar": "application/x-tar",
	".xz":  "application/x-xz",
	// video
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mov":  "video/quicktime",
	".avi":  "video/x-msvideo",
	".webm": "video/webm",
	".ts":   "video/mp2t",
	".wmv":  "video/x-ms-wmv",
	// audio
	".mp3":  "audio/mpeg",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".m4b":  "audio/mp4",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".wav":  "audio/wav",
	".aac":  "audio/aac",
	// subtitles and text
	".srt": "application/x-subrip",
	".vtt": "text/vtt",
	".ass": "text/plain; charset=utf-8",
	".ssa": "text/plain; charset=utf-8",
	".sub": "text/plain; charset=utf-8",
	".nfo": "text/plain; charset=utf-8",
	".txt": "text/plain; charset=utf-8",
	".sfv": "text/plain; charset=utf-8",
	".md5": "text/plain; charset=utf-8",
	// documents and images
	".pdf":  "application/pdf",
	".epub": "application/epub+zip",
	".mobi": "application/x-mobipocket-ebook",
	".cbz":  "application/vnd.comicbook+zip",
	".cbr":  "application/vnd.comicbook-rar",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// ForPath returns the media type for a file name or relative path.
func ForPath(p string) string {
	if t, ok := byExt[strings.ToLower(path.Ext(p))]; ok {
		return t
	}
	return Default
}
