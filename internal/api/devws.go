package api

// Dev-workspace and credential types (docs/M7.md §3, §4). Every params type has Validate, and
// DecodeValid is the one way a handler decodes them: exact-case keys, no unknown or duplicate
// keys, no trailing data, then Validate. Values never appear in an error message.
//
// The credential methods are admin-only (IsAdminMethod); operators never reach them on api@.
// credential.set carries the secret as ONE body frame after the request (Length bytes, SHA-256
// SHA256), never in params; no result or list ever echoes a value.

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
)

// Validator is implemented by every params type below.
type Validator interface{ Validate() *Error }

// DecodeExact strictly decodes params into v (exact-case keys, no unknown or duplicate keys, no
// trailing data). Missing or null params decode as an empty object, so mandatory fields fail in
// Validate rather than being silently skipped.
func DecodeExact(raw json.RawMessage, v any) *Error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := transport.StrictUnmarshal(raw, v); err != nil {
		return Errorf(CodeBadRequest, "invalid params")
	}
	return nil
}

// DecodeValid decodes with DecodeExact and then validates.
func DecodeValid(raw json.RawMessage, v Validator) *Error {
	if e := DecodeExact(raw, v); e != nil {
		return e
	}
	return v.Validate()
}

func badRequest(err error) *Error {
	if err == nil {
		return nil
	}
	return Errorf(CodeBadRequest, "%v", err)
}

var (
	idRE      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	projectRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,256}$`)
)

func checkWorkspace(id string) *Error {
	if !idRE.MatchString(id) {
		return Errorf(CodeBadRequest, "invalid workspace")
	}
	return nil
}

func checkConfirm(c string) *Error {
	if len(c) > 128 {
		return Errorf(CodeBadRequest, "invalid confirm")
	}
	return nil
}

// ---- dev workspace: operator methods --------------------------------------------------------

// FSParams is workspace.fs: one file-tool call (op is one of transport.FSOps; unknown ops are
// refused). A write is followed by one body frame of Length bytes whose SHA-256 is SHA256.
// Confirm must equal the fleet name on guarded fleets for the mutating ops (see
// transport.FSMutating).
type FSParams struct {
	Workspace string `json:"workspace"`
	Confirm   string `json:"confirm,omitempty"`
	transport.FSOp
}

// FSResult is the workspace.fs result; for an image, Length raw bytes follow as one body frame.
type FSResult = transport.FSResult

// FSEntry is one ls/glob/stat entry.
type FSEntry = transport.FSEntry

// Validate implements Validator.
func (p *FSParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	if e := checkConfirm(p.Confirm); e != nil {
		return e
	}
	return badRequest(p.FSOp.Validate())
}

// NeedsConfirm reports whether the call mutates files.
func (p *FSParams) NeedsConfirm() bool { return transport.FSMutating(p.Op) }

// MaxJobTimeoutS is the largest job timeout a caller may request (the runner's job_max applies too).
const MaxJobTimeoutS = transport.MaxJobTimeoutS

// JobStartParams is workspace.job_start: start a background shell (bash -lc Command).
type JobStartParams struct {
	Workspace   string `json:"workspace"`
	Command     string `json:"command"`
	Description string `json:"description,omitempty"`
	TimeoutS    int    `json:"timeout_s,omitempty"` // 0 = the runner's job_max
	Confirm     string `json:"confirm,omitempty"`
}

// Op returns the transport job op.
func (p *JobStartParams) Op() transport.JobOp {
	return transport.JobOp{Command: p.Command, Description: p.Description, TimeoutS: p.TimeoutS}
}

// Validate implements Validator.
func (p *JobStartParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	if e := checkConfirm(p.Confirm); e != nil {
		return e
	}
	op := p.Op()
	return badRequest(op.ValidateFor(transport.OpJobStart))
}

// JobResult describes one job; JobStatusResult lists them.
type JobResult = transport.JobResult

// JobStartResult is the workspace.job_start result.
type JobStartResult struct {
	Job JobResult `json:"job"`
}

// JobStatusParams is workspace.job_status. An empty Job lists the session's jobs.
type JobStatusParams struct {
	Workspace string `json:"workspace"`
	Job       string `json:"job,omitempty"`
}

// Validate implements Validator.
func (p *JobStatusParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	op := transport.JobOp{Job: p.Job}
	return badRequest(op.ValidateFor(transport.OpJobStatus))
}

// JobStatusResult: Job when one was asked for, Jobs when listing.
type JobStatusResult struct {
	Job  *JobResult  `json:"job,omitempty"`
	Jobs []JobResult `json:"jobs,omitempty"`
}

// JobTailParams is workspace.job_tail: the output produced since the previous tail of this job.
type JobTailParams struct {
	Workspace string `json:"workspace"`
	Job       string `json:"job"`
	Filter    string `json:"filter,omitempty"` // RE2; only matching lines
}

// Validate implements Validator.
func (p *JobTailParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	op := transport.JobOp{Job: p.Job, Filter: p.Filter}
	return badRequest(op.ValidateFor(transport.OpJobTail))
}

// JobTailResult carries the new output in Job.Output.
type JobTailResult struct {
	Job JobResult `json:"job"`
}

// JobCancelParams is workspace.job_cancel: kill the job's process group.
type JobCancelParams struct {
	Workspace string `json:"workspace"`
	Job       string `json:"job"`
}

// Validate implements Validator.
func (p *JobCancelParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	op := transport.JobOp{Job: p.Job}
	return badRequest(op.ValidateFor(transport.OpJobCancel))
}

// JobCancelResult is the job after the cancel.
type JobCancelResult struct {
	Job JobResult `json:"job"`
}

// OutputReadParams is workspace.output_read: page saved output by Ref (a foreground command) or
// by Job (and Stream). See transport.OutputOp.
type OutputReadParams struct {
	Workspace string `json:"workspace"`
	transport.OutputOp
}

// Validate implements Validator.
func (p *OutputReadParams) Validate() *Error {
	if e := checkWorkspace(p.Workspace); e != nil {
		return e
	}
	return badRequest(p.OutputOp.Validate())
}

// OutputReadResult is one page of saved output.
type OutputReadResult = transport.OutputResult

// ValidateCredentialNames checks the credentials list of workspace.open: at most
// transport.MaxSyncCreds valid, unique names.
func ValidateCredentialNames(names []string) *Error {
	if len(names) > transport.MaxSyncCreds {
		return Errorf(CodeBadRequest, "too many credentials")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if !transport.ValidCredName(n) || seen[n] {
			return Errorf(CodeBadRequest, "invalid credentials list")
		}
		seen[n] = true
	}
	return nil
}

// RunnerCredential is what an operator sees of a credential a runner holds: name, kind, hosts and
// status (ok, expiring, expired, invalid). No fingerprint, no expiry, no value.
type RunnerCredential struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Hosts  []string `json:"hosts"`
	Status string   `json:"status"`
}

// ---- credentials: admin methods -------------------------------------------------------------

// CredentialSetParams is credential.set: create or replace (by Name; replacing bumps the version
// and re-delivers to every assigned runner). One body frame of Length bytes follows holding the
// secret (the OpenSSH private key, token or password) with SHA-256 SHA256. Rotate "gitlab_self"
// (https-token only) makes central rotate the token before Expires; RotateProject then names the
// GitLab project (id or path).
type CredentialSetParams struct {
	transport.CredMeta
	Expires       *time.Time `json:"expires,omitempty"`
	Rotate        string     `json:"rotate,omitempty"`
	RotateProject string     `json:"rotate_project,omitempty"`
	Length        int        `json:"length"`
	SHA256        string     `json:"sha256"`
}

// RotateGitLabSelf is the only Rotate value.
const RotateGitLabSelf = "gitlab_self"

// MaxCredentialValue is the largest credential secret.
const MaxCredentialValue = transport.MaxCredentialData

// Validate implements Validator.
func (p *CredentialSetParams) Validate() *Error {
	if err := p.CredMeta.Validate(); err != nil {
		return badRequest(err)
	}
	switch p.Rotate {
	case "":
		if p.RotateProject != "" {
			return Errorf(CodeBadRequest, "rotate_project needs rotate")
		}
	case RotateGitLabSelf:
		if p.Kind != transport.CredHTTPSToken {
			return Errorf(CodeBadRequest, "rotate is only valid for https-token")
		}
		if !projectRE.MatchString(p.RotateProject) {
			return Errorf(CodeBadRequest, "rotate_project required")
		}
		if p.Expires == nil {
			return Errorf(CodeBadRequest, "expires required with rotate")
		}
	default:
		return Errorf(CodeBadRequest, "invalid rotate")
	}
	if p.Length < 1 || p.Length > MaxCredentialValue {
		return Errorf(CodeBadRequest, "length must be 1-%d", MaxCredentialValue)
	}
	if !transport.ValidSHA256(p.SHA256) {
		return Errorf(CodeBadRequest, "sha256 must be 64 lowercase hex characters")
	}
	return nil
}

// CredentialAssignment is one assignment: a whole fleet (Runner empty) or one runner.
type CredentialAssignment struct {
	Fleet  string `json:"fleet"`
	Runner string `json:"runner,omitempty"`
}

// CredentialDelivery is the delivery status of a credential on one runner: ok, expiring, expired,
// invalid as the runner reports, or "pending" before it has confirmed this Version.
type CredentialDelivery struct {
	Fleet   string `json:"fleet"`
	Runner  string `json:"runner"`
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

// CredentialInfo is the masked view of a credential (admin only). Fingerprint is the SHA-256 of
// the public key (ssh-key) or of the value (others), never the value itself.
type CredentialInfo struct {
	Name          string                 `json:"name"`
	Kind          string                 `json:"kind"`
	Hosts         []string               `json:"hosts"`
	User          string                 `json:"user,omitempty"`
	Auth          string                 `json:"auth,omitempty"`
	Header        string                 `json:"header,omitempty"`
	HostBinding   string                 `json:"host_binding,omitempty"`
	Rotate        string                 `json:"rotate,omitempty"`
	RotateProject string                 `json:"rotate_project,omitempty"`
	Version       int64                  `json:"version"`
	Fingerprint   string                 `json:"fingerprint,omitempty"`
	Expires       *time.Time             `json:"expires,omitempty"`
	Created       time.Time              `json:"created"`
	Updated       time.Time              `json:"updated"`
	Assignments   []CredentialAssignment `json:"assignments,omitempty"`
	Delivery      []CredentialDelivery   `json:"delivery,omitempty"`
	// RotateError is the last failed central-side rotation (fixed text, never a value).
	RotateError string `json:"rotate_error,omitempty"`
}

// CredentialSetResult is the credential.set result.
type CredentialSetResult struct {
	Credential CredentialInfo `json:"credential"`
	Replaced   bool           `json:"replaced"`
}

// CredentialListParams is credential.list; an empty Name lists all.
type CredentialListParams struct {
	Name string `json:"name,omitempty"`
}

// Validate implements Validator.
func (p *CredentialListParams) Validate() *Error {
	if p.Name != "" && !transport.ValidCredName(p.Name) {
		return Errorf(CodeBadRequest, "invalid credential name")
	}
	return nil
}

// CredentialListResult is the credential.list result.
type CredentialListResult struct {
	Credentials []CredentialInfo `json:"credentials"`
}

// CredentialDeleteParams is credential.delete.
type CredentialDeleteParams struct {
	Name string `json:"name"`
}

// Validate implements Validator.
func (p *CredentialDeleteParams) Validate() *Error { return checkCredName(p.Name) }

func checkCredName(n string) *Error {
	if !transport.ValidCredName(n) {
		return Errorf(CodeBadRequest, "invalid credential name")
	}
	return nil
}

// CredentialAssignParams is credential.assign: to one runner (Fleet and Runner) or a whole fleet
// (Fleet only).
type CredentialAssignParams struct {
	Name   string `json:"name"`
	Fleet  string `json:"fleet"`
	Runner string `json:"runner,omitempty"`
}

// CredentialUnassignParams is credential.unassign; same shape as assign.
type CredentialUnassignParams struct {
	Name   string `json:"name"`
	Fleet  string `json:"fleet"`
	Runner string `json:"runner,omitempty"`
}

func validateAssign(name, fleet, runner string) *Error {
	if e := checkCredName(name); e != nil {
		return e
	}
	if !nameRE.MatchString(fleet) || (runner != "" && !nameRE.MatchString(runner)) {
		return Errorf(CodeBadRequest, "invalid fleet or runner name")
	}
	return nil
}

// Validate implements Validator.
func (p *CredentialAssignParams) Validate() *Error { return validateAssign(p.Name, p.Fleet, p.Runner) }

// Validate implements Validator.
func (p *CredentialUnassignParams) Validate() *Error {
	return validateAssign(p.Name, p.Fleet, p.Runner)
}

// CredentialResult is the result of assign, unassign and rotate.
type CredentialResult struct {
	Credential CredentialInfo `json:"credential"`
}

// CredentialRotateParams is credential.rotate: rotate now (rotate=gitlab_self credentials only).
type CredentialRotateParams struct {
	Name string `json:"name"`
}

// Validate implements Validator.
func (p *CredentialRotateParams) Validate() *Error { return checkCredName(p.Name) }
