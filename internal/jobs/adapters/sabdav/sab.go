package sabdav

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"mountenant/internal/jobs/domain"
)

// sabResponse covers the fields Mountenant reads from SABnzbd API responses.
// AltMount omits "status" on success for some modes and reports errors as
// HTTP 200 with status=false; NzbDav uses 401/500 with status=false.
type sabResponse struct {
	Status  *bool       `json:"status"`
	Error   *string     `json:"error"`
	NzoIDs  []string    `json:"nzo_ids"`
	Version string      `json:"version"`
	Queue   *sabQueue   `json:"queue"`
	History *sabHistory `json:"history"`
}

type sabQueue struct {
	Slots []sabSlot `json:"slots"`
}

type sabHistory struct {
	Slots []sabSlot `json:"slots"`
}

// sabSlot is a queue or history row. Queue rows carry "filename", history rows
// "name"; NzbDav's queue filename keeps the ".nzb" suffix.
type sabSlot struct {
	NzoID        string  `json:"nzo_id"`
	Name         string  `json:"name"`
	Filename     string  `json:"filename"`
	Category     string  `json:"category"`
	Cat          string  `json:"cat"`
	Status       string  `json:"status"`
	FailMessage  *string `json:"fail_message"`
	CompleteTime int64   `json:"completetime"`
}

func (s sabSlot) jobName() string {
	n := s.Name
	if n == "" {
		n = s.Filename
	}
	return strings.TrimSuffix(n, ".nzb")
}

func (s sabSlot) category() string {
	if s.Category != "" {
		return s.Category
	}
	return s.Cat
}

func (s sabSlot) failMessage() string {
	if s.FailMessage == nil {
		return ""
	}
	return *s.FailMessage
}

// sabCall performs one SABnzbd API read and decodes the response, retrying
// transient failures.
func (a *Adapter) sabCall(ctx context.Context, params url.Values, body io.Reader, contentType string) (*sabResponse, error) {
	return a.sabDo(ctx, params, body, contentType, body == nil)
}

// sabOnce performs a SABnzbd API request without retries, for mutations.
func (a *Adapter) sabOnce(ctx context.Context, params url.Values) (*sabResponse, error) {
	return a.sabDo(ctx, params, nil, "", false)
}

func (a *Adapter) sabDo(ctx context.Context, params url.Values, body io.Reader, contentType string, retry bool) (*sabResponse, error) {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("output", "json")
	q.Set("apikey", a.cfg.APIKey)
	u := a.cfg.APIURL + a.profile.APIPath + "?" + q.Encode()

	do := func() (*sabResponse, error) {
		method := http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrBackendUnavailable, err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSABResponse))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrBackendUnavailable, err)
		}
		var out sabResponse
		jsonErr := json.Unmarshal(raw, &out)
		if out.Status != nil && !*out.Status {
			msg := ""
			if out.Error != nil {
				msg = *out.Error
			}
			if resp.StatusCode >= 500 {
				return nil, fmt.Errorf("%w: sab %s: HTTP %d: %s", domain.ErrBackendUnavailable, params.Get("mode"), resp.StatusCode, msg)
			}
			return nil, fmt.Errorf("%w: sab %s: %s", domain.ErrBackendRejected, params.Get("mode"), msg)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("%w: sab %s: HTTP %d", domain.ErrBackendRejected, params.Get("mode"), resp.StatusCode)
		case resp.StatusCode >= 500:
			return nil, fmt.Errorf("%w: sab %s: HTTP %d", domain.ErrBackendUnavailable, params.Get("mode"), resp.StatusCode)
		case resp.StatusCode >= 300:
			return nil, fmt.Errorf("%w: sab %s: HTTP %d", domain.ErrBadResponse, params.Get("mode"), resp.StatusCode)
		case jsonErr != nil:
			return nil, fmt.Errorf("%w: sab %s: %v", domain.ErrBadResponse, params.Get("mode"), jsonErr)
		}
		return &out, nil
	}

	if !retry {
		return do()
	}
	var out *sabResponse
	err := a.retry(ctx, func() error {
		var err error
		out, err = do()
		return err
	})
	return out, err
}

const maxSABResponse = 32 << 20

func (a *Adapter) addFile(ctx context.Context, nzb []byte, jobID string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	// Both products name the job directory after this filename (ADR 0002).
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="name"; filename="%s.nzb"`, jobID))
	h.Set("Content-Type", "application/x-nzb")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(nzb); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	resp, err := a.sabCall(ctx, url.Values{
		"mode":    {"addfile"},
		"cat":     {a.cfg.Category},
		"nzbname": {jobID}, // AltMount uses it as nzo_id; NzbDav ignores it
	}, &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	if len(resp.NzoIDs) == 0 || resp.NzoIDs[0] == "" {
		return "", fmt.Errorf("%w: addfile returned no nzo_id", domain.ErrBadResponse)
	}
	return resp.NzoIDs[0], nil
}

func (a *Adapter) queueSlots(ctx context.Context) ([]sabSlot, error) {
	resp, err := a.sabCall(ctx, url.Values{"mode": {"queue"}, "category": {a.cfg.Category}}, nil, "")
	if err != nil {
		return nil, err
	}
	if resp.Queue == nil {
		return nil, nil
	}
	return a.inCategory(resp.Queue.Slots), nil
}

func (a *Adapter) historySlots(ctx context.Context, nzoID string) ([]sabSlot, error) {
	params := url.Values{"mode": {"history"}, "category": {a.cfg.Category}}
	if nzoID != "" {
		params.Set("nzo_ids", nzoID)
	}
	resp, err := a.sabCall(ctx, params, nil, "")
	if err != nil {
		return nil, err
	}
	if resp.History == nil {
		return nil, nil
	}
	slots := a.inCategory(resp.History.Slots)
	if nzoID == "" {
		return slots, nil
	}
	// Filter client-side as well; not every product honours every filter.
	var out []sabSlot
	for _, s := range slots {
		if s.NzoID == nzoID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (a *Adapter) inCategory(slots []sabSlot) []sabSlot {
	var out []sabSlot
	for _, s := range slots {
		if c := s.category(); c == "" || c == a.cfg.Category {
			out = append(out, s)
		}
	}
	return out
}
