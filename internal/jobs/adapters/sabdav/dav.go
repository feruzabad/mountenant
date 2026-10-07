package sabdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

type davEntry struct {
	Path    string // decoded URL path, no trailing slash
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type multistatus struct {
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Href      string        `xml:"DAV: href"`
	Propstats []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string `xml:"DAV: status"`
	Prop   struct {
		Length       string `xml:"DAV: getcontentlength"`
		LastModified string `xml:"DAV: getlastmodified"`
		ResourceType struct {
			Collection *struct{} `xml:"DAV: collection"`
		} `xml:"DAV: resourcetype"`
	} `xml:"DAV: prop"`
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:getcontentlength/><D:getlastmodified/></D:prop></D:propfind>`

// propfind lists dirURL. A 404 returns ErrContentMissing. The backend's Date
// header is returned so callers can compare backend timestamps with the
// backend clock only.
func (a *Adapter) propfind(ctx context.Context, dirURL, depth string) ([]davEntry, time.Time, error) {
	var entries []davEntry
	var backendNow time.Time
	err := a.retry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, "PROPFIND", dirURL, strings.NewReader(propfindBody))
		if err != nil {
			return err
		}
		req.Header.Set("Depth", depth)
		req.Header.Set("Content-Type", "application/xml")
		req.SetBasicAuth(a.cfg.DavUser, a.cfg.DavPassword)
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %w", domain.ErrBackendUnavailable, err)
		}
		defer resp.Body.Close()
		if err := davStatusErr("PROPFIND", resp.StatusCode); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusMultiStatus {
			return fmt.Errorf("%w: PROPFIND: HTTP %d", domain.ErrBadResponse, resp.StatusCode)
		}
		backendNow, _ = http.ParseTime(resp.Header.Get("Date"))
		var ms multistatus
		if err := xml.NewDecoder(io.LimitReader(resp.Body, maxPropfind)).Decode(&ms); err != nil {
			return fmt.Errorf("%w: PROPFIND: %w", domain.ErrBadResponse, err)
		}
		entries = entries[:0]
		for _, r := range ms.Responses {
			e, ok, err := parseResponse(r.Href, r.Propstats)
			if err != nil {
				return err
			}
			if ok {
				entries = append(entries, e)
			}
		}
		return nil
	})
	return entries, backendNow, err
}

const maxPropfind = 64 << 20

func parseResponse(href string, propstats []davPropstat) (davEntry, bool, error) {
	// NzbDav returns absolute URLs with its internal host
	// (http://localhost:8080/content/...); AltMount returns paths. Only the
	// decoded path is meaningful.
	u, err := url.Parse(href)
	if err != nil {
		return davEntry{}, false, fmt.Errorf("%w: bad href %q", domain.ErrBadResponse, href)
	}
	e := davEntry{Path: strings.TrimSuffix(u.Path, "/")}
	found := false
	for _, ps := range propstats {
		if !strings.Contains(ps.Status, " 200 ") {
			continue
		}
		found = true
		if ps.Prop.ResourceType.Collection != nil {
			e.IsDir = true
		}
		if ps.Prop.Length != "" {
			e.Size, _ = strconv.ParseInt(strings.TrimSpace(ps.Prop.Length), 10, 64)
		}
		if ps.Prop.LastModified != "" {
			e.ModTime, _ = http.ParseTime(ps.Prop.LastModified)
		}
	}
	return e, found, nil
}

func davStatusErr(op string, code int) error {
	switch {
	case code == http.StatusNotFound:
		return fmt.Errorf("%w: %s: HTTP 404", domain.ErrContentMissing, op)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: %s: HTTP %d", domain.ErrBackendRejected, op, code)
	case code >= 500:
		return fmt.Errorf("%w: %s: HTTP %d", domain.ErrBackendUnavailable, op, code)
	}
	return nil
}

// davDelete deletes a collection or file. 404 counts as success.
func (a *Adapter) davDelete(ctx context.Context, target string) error {
	return a.retry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(a.cfg.DavUser, a.cfg.DavPassword)
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %w", domain.ErrBackendUnavailable, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if err := davStatusErr("DELETE", resp.StatusCode); err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%w: DELETE: HTTP %d", domain.ErrBadResponse, resp.StatusCode)
		}
		return nil
	})
}

// davGet opens a file with at most one closed range and validates that the
// response matches the request exactly before handing out the body.
func (a *Adapter) davGet(ctx context.Context, fileURL string, r *domain.ByteRange) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := a.retry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(a.cfg.DavUser, a.cfg.DavPassword)
		req.Header.Set("Accept-Encoding", "identity")
		if r != nil {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.Start, r.End))
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %w", domain.ErrBackendUnavailable, err)
		}
		if err := davStatusErr("GET", resp.StatusCode); err != nil {
			resp.Body.Close()
			return err
		}
		if err := validateGet(resp, r); err != nil {
			resp.Body.Close()
			return err
		}
		body = resp.Body
		return nil
	})
	return body, err
}

func validateGet(resp *http.Response, r *domain.ByteRange) error {
	if r == nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%w: GET: want 200, got %d", domain.ErrBadResponse, resp.StatusCode)
		}
		return nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("%w: GET range: want 206, got %d", domain.ErrBadResponse, resp.StatusCode)
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return fmt.Errorf("%w: GET range: bad Content-Range %q", domain.ErrBadResponse, resp.Header.Get("Content-Range"))
	}
	if start != r.Start || end != r.End {
		return fmt.Errorf("%w: GET range: asked %d-%d, got %d-%d", domain.ErrBadResponse, r.Start, r.End, start, end)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != r.Len() {
		return fmt.Errorf("%w: GET range: Content-Length %d, want %d", domain.ErrBadResponse, resp.ContentLength, r.Len())
	}
	return nil
}
