package sabdav

import (
	"net/url"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// Profile captures what differs between products that speak the SABnzbd API
// plus WebDAV. Data plus small functions, never a fork of the adapter
// (specification §10.1). Values were verified against the pinned versions in
// docs/feasibility.md.
type Profile struct {
	Name string

	// APIPath is the SABnzbd API path below the API base URL.
	APIPath string

	// ContentRoot is the WebDAV path below the WebDAV base URL that holds one
	// directory per category.
	ContentRoot string

	// ReadyMode is the SABnzbd mode used by Ping. It must require the API key.
	ReadyMode string

	// DeleteParams are extra query parameters for mode=history&name=delete
	// that make the backend remove content, if it supports that.
	DeleteParams url.Values

	// StatusMap maps backend status strings to backend states. Unlisted
	// values map to BackendUnknown.
	StatusMap map[string]domain.BackendState
}

// AltMount is the profile for AltMount 0.3.2.
//
// Required backend settings: sabnzbd.enabled, the namespace category in
// sabnzbd.categories, import.allowed_file_extensions=[],
// import.expand_bluray_iso=false, import.rename_to_nzb_name=false.
var AltMount = Profile{
	Name:        "altmount",
	APIPath:     "/sabnzbd/api",
	ContentRoot: "/webdav/complete",
	ReadyMode:   "version", // requires the key on AltMount
	// History delete never removes content; the adapter's WebDAV DELETE does.
	DeleteParams: url.Values{},
	StatusMap: map[string]domain.BackendState{
		"Queued":      domain.BackendQueued,
		"Paused":      domain.BackendQueued,
		"Downloading": domain.BackendImporting,
		// AltMount reports pending items as "Unknown" in history.
		"Unknown":   domain.BackendImporting,
		"Completed": domain.BackendCompleted,
		"Failed":    domain.BackendFailed,
	},
}

// NzbDav is the profile for NzbDav 0.6.4.
//
// Required backend settings: api.ensure-importable-video=false,
// webdav.enforce-readonly=false.
var NzbDav = Profile{
	Name:        "nzbdav",
	APIPath:     "/api",
	ContentRoot: "/content",
	ReadyMode:   "queue", // mode=version answers without a key
	// The standard del_files is ignored; this non-standard flag removes content.
	DeleteParams: url.Values{"del_completed_files": {"1"}},
	StatusMap: map[string]domain.BackendState{
		"Queued":      domain.BackendQueued,
		"Downloading": domain.BackendImporting,
		"Completed":   domain.BackendCompleted,
		"Failed":      domain.BackendFailed,
	},
}

// ProfileByName returns the profile for a backend.type config value.
func ProfileByName(name string) (Profile, bool) {
	switch name {
	case AltMount.Name:
		return AltMount, true
	case NzbDav.Name:
		return NzbDav, true
	}
	return Profile{}, false
}
