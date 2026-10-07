package app

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// fakeBackend is a scriptable domain.Backend.
type fakeBackend struct {
	mu        sync.Mutex
	submitErr error
	submitted map[string][]byte // jobID → NZB
	status    map[string]domain.BackendStatus
	files     map[string][]domain.BackendFile
	listErr   error
	present   map[string]bool // job directories on the backend
	deleteErr error
	sticky    bool // Delete does not remove anything
	calls     []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{submitted: map[string][]byte{}, status: map[string]domain.BackendStatus{}, files: map[string][]domain.BackendFile{}, present: map[string]bool{}}
}

func (f *fakeBackend) call(c string) { f.calls = append(f.calls, c) }

func (f *fakeBackend) Ping(context.Context) error { return nil }

func (f *fakeBackend) Submit(_ context.Context, nzb []byte, jobID string) (domain.BackendRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("submit " + jobID)
	if f.submitErr != nil {
		return domain.BackendRef{}, f.submitErr
	}
	f.submitted[jobID] = nzb
	f.present[jobID] = true
	f.status[jobID] = domain.BackendStatus{State: domain.BackendQueued}
	return domain.BackendRef{JobID: jobID, NzoID: "nzo-" + jobID}, nil
}

func (f *fakeBackend) Find(_ context.Context, jobID string) (domain.BackendRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("find " + jobID)
	if _, ok := f.submitted[jobID]; ok {
		return domain.BackendRef{JobID: jobID, NzoID: "nzo-" + jobID}, true, nil
	}
	return domain.BackendRef{}, false, nil
}

func (f *fakeBackend) Status(_ context.Context, ref domain.BackendRef) (domain.BackendStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[ref.JobID]
	if !ok {
		return domain.BackendStatus{State: domain.BackendNotFound}, nil
	}
	return st, nil
}

func (f *fakeBackend) ListFiles(_ context.Context, ref domain.BackendRef) ([]domain.BackendFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !f.present[ref.JobID] {
		return nil, domain.ErrContentMissing
	}
	return f.files[ref.JobID], nil
}

func (f *fakeBackend) Open(context.Context, domain.BackendRef, string, *domain.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeBackend) Delete(_ context.Context, ref domain.BackendRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("delete " + ref.JobID + " " + ref.NzoID)
	if !f.sticky {
		delete(f.present, ref.JobID)
		delete(f.status, ref.JobID)
		delete(f.submitted, ref.JobID)
	}
	return f.deleteErr
}

func (f *fakeBackend) VerifyGone(_ context.Context, ref domain.BackendRef) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, inHistory := f.status[ref.JobID]
	return !f.present[ref.JobID] && !inHistory, nil
}

func (f *fakeBackend) ListNamespace(context.Context) (domain.NamespaceListing, error) {
	return domain.NamespaceListing{}, nil
}

func (f *fakeBackend) set(jobID string, st domain.BackendState, files ...domain.BackendFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[jobID] = domain.BackendStatus{State: st, FailMessage: "articles missing"}
	if files != nil {
		f.files[jobID] = files
	}
}
