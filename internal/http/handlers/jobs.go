package handlers

import (
	"context"
	"errors"
	"io"

	"github.com/feruzabad/mountenant/internal/http/api"
	jobsapp "github.com/feruzabad/mountenant/internal/jobs/app"
	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// Jobs is the part of the Jobs context the handlers use.
type Jobs interface {
	Submit(ctx context.Context, in jobsapp.SubmitInput) (jobsapp.SubmitResult, error)
	List(ctx context.Context, owner domain.OwnerID, q domain.ListQuery) ([]*domain.Job, string, error)
	Get(ctx context.Context, owner domain.OwnerID, id domain.JobID) (*domain.Job, error)
	Delete(ctx context.Context, owner domain.OwnerID, id domain.JobID) error
}

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

func (s *Server) owner(ctx context.Context) (domain.OwnerID, domain.Limits) {
	u := principalFrom(ctx).user
	return domain.OwnerID(u.ID), domain.Limits{
		MaxActiveJobs: u.Quota.MaxActiveJobs,
		MaxTotalJobs:  u.Quota.MaxTotalJobs,
		MaxNZBBytes:   u.Quota.MaxNZBBytes,
	}
}

// SubmitJob implements UC-10. The "nzb" part is streamed into the intake;
// the request was already bounded by the guard.
func (s *Server) SubmitJob(ctx context.Context, req api.SubmitJobRequestObject) (api.SubmitJobResponseObject, error) {
	if req.Body == nil {
		return nil, badRequest("multipart field \"nzb\" is required")
	}
	for {
		part, err := req.Body.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, badRequest("multipart field \"nzb\" is required")
		}
		if err != nil {
			return nil, badRequest("malformed multipart body")
		}
		if part.FormName() != "nzb" {
			continue
		}
		owner, limits := s.owner(ctx)
		res, err := s.Jobs.Submit(ctx, jobsapp.SubmitInput{Owner: owner, Limits: limits, FileName: part.FileName(), Body: part})
		if err != nil {
			return nil, err
		}
		body := api.SubmittedJob{Job: toAPIJob(res.Job), Duplicate: res.Duplicate}
		if res.Duplicate {
			return api.SubmitJob200JSONResponse(body), nil
		}
		return api.SubmitJob201JSONResponse(body), nil
	}
}

// ListJobs implements UC-11.
func (s *Server) ListJobs(ctx context.Context, req api.ListJobsRequestObject) (api.ListJobsResponseObject, error) {
	q := domain.ListQuery{Limit: defaultPageSize}
	if p := req.Params; p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > maxPageSize {
			return nil, badRequest("limit must be between 1 and 100")
		}
		q.Limit = *p.Limit
	}
	if req.Params.Cursor != nil {
		q.Cursor = *req.Params.Cursor
	}
	if st := req.Params.Status; st != nil {
		if !st.Valid() {
			return nil, badRequest("unknown status")
		}
		q.Status = domain.Status(*st)
	}
	owner, _ := s.owner(ctx)
	jobs, next, err := s.Jobs.List(ctx, owner, q)
	if err != nil {
		return nil, err
	}
	page := api.JobPage{Items: make([]api.Job, 0, len(jobs))}
	for _, j := range jobs {
		page.Items = append(page.Items, toAPIJob(j))
	}
	if next != "" {
		page.NextCursor = &next
	}
	return api.ListJobs200JSONResponse(page), nil
}

// GetJob implements UC-12.
func (s *Server) GetJob(ctx context.Context, req api.GetJobRequestObject) (api.GetJobResponseObject, error) {
	owner, _ := s.owner(ctx)
	j, err := s.Jobs.Get(ctx, owner, domain.JobID(req.JobId))
	if err != nil {
		return nil, err
	}
	return api.GetJob200JSONResponse(toAPIJob(j)), nil
}

// DeleteJob implements UC-13.
func (s *Server) DeleteJob(ctx context.Context, req api.DeleteJobRequestObject) (api.DeleteJobResponseObject, error) {
	owner, _ := s.owner(ctx)
	if err := s.Jobs.Delete(ctx, owner, domain.JobID(req.JobId)); err != nil {
		return nil, err
	}
	return api.DeleteJob202Response{}, nil
}

// ToAPIJob is the Job representation of spec §6.3, also used for SSE.
func toAPIJob(j *domain.Job) api.Job {
	out := api.Job{
		Id:        string(j.ID),
		Name:      j.NZBName,
		Status:    api.JobStatus(j.Status),
		CreatedAt: j.CreatedAt.UTC(),
		ExpiresAt: j.ExpiresAt.UTC(),
		Files:     make([]api.JobFile, 0, len(j.Files)),
	}
	if !j.ReadyAt.IsZero() {
		t := j.ReadyAt.UTC()
		out.ReadyAt = &t
	}
	if j.Failure != nil {
		out.Failure = &api.JobFailure{Code: j.Failure.Code, Message: j.Failure.Message}
	}
	for _, f := range j.Files {
		out.Files = append(out.Files, api.JobFile{Path: f.RelPath, Size: f.Size, ContentType: f.ContentType})
	}
	return out
}
