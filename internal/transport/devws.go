package transport

// Dev-workspace wire types: the file tools (fs), background jobs, output
// paging and credential delivery (creds_sync). Everything here is validated twice, by central
// before it sends and by the runner on receipt, with the same code. Keys are exact-case, unknown
// and duplicate keys are refused, and every field that is not valid for an op is refused.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
)

// ---- exact-case key check -------------------------------------------------------------------

var unmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func jsonFields(t reflect.Type, out map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				jsonFields(ft, out)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
}

// checkExactKeys walks raw alongside type t and refuses any object key that is not the exact
// (case-sensitive) JSON name of a field, and any duplicate key. encoding/json alone matches keys
// case-insensitively and takes the last duplicate.
func checkExactKeys(b []byte, t reflect.Type) error {
	return checkKeys(json.RawMessage(b), t)
}

func checkKeys(raw json.RawMessage, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if raw[0] != '{' {
			return nil // the real decode reports the type error
		}
		fields := map[string]reflect.Type{}
		jsonFields(t, fields)
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return err
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			var val json.RawMessage
			if err := dec.Decode(&val); err != nil {
				return err
			}
			ft, ok := fields[key]
			if !ok {
				return errors.New("unknown field")
			}
			if seen[key] {
				return errors.New("duplicate field")
			}
			seen[key] = true
			if err := checkKeys(val, ft); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if raw[0] != '[' || t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		for _, it := range items {
			if err := checkKeys(it, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		if raw[0] != '{' {
			return nil
		}
		var items map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		for _, it := range items {
			if err := checkKeys(it, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// StrictUnmarshal decodes exactly one JSON value into v: exact-case keys, no unknown or duplicate
// keys, no trailing data.
func StrictUnmarshal(b []byte, v any) error { return strictUnmarshal(b, v) }

// ---- shared limits --------------------------------------------------------------------------

const (
	MaxEditString     = 24 << 10 // old_string, new_string, new_source
	MaxEdits          = 64
	MaxEditsBytes     = 48 << 10 // sum of all strings of one multi_edit
	MaxPatternBytes   = 4 << 10
	MaxGlobBytes      = 1 << 10
	MaxIgnoreGlobs    = 32
	MaxLines          = 100000 // lines per read/tail, head_limit
	MaxContextLines   = 100
	MaxCommandBytes   = 32 << 10
	MaxDescription    = 256
	MaxJobTimeoutS    = 24 * 60 * 60
	MaxJobList        = 64
	MaxRefBytes       = 64
	MaxCredentialData = 16 << 10 // one credential value
	MaxSyncCreds      = 32       // credentials per creds_sync (whole body stays under MaxBodyBytes)
	MaxCredHosts      = 16
	MaxKnownHosts     = 16
	MaxKnownHostBytes = 1 << 10
	MaxFSOutputText   = MaxBodyBytes
)

var (
	jobIDRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	refRE     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	typeRE    = regexp.MustCompile(`^[A-Za-z0-9+_-]{1,32}$`)
	credName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sshUserRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	headerRE  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	hostRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	hex64RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidSHA256 reports whether s is 64 lowercase hex characters.
func ValidSHA256(s string) bool { return hex64RE.MatchString(s) }

func checkRE2(what, s string, max int) error {
	if err := checkText(what, s, max); err != nil {
		return err
	}
	if s == "" {
		return fmt.Errorf("%s required", what)
	}
	if _, err := regexp.Compile(s); err != nil {
		return fmt.Errorf("%s is not a valid RE2 expression", what)
	}
	return nil
}

// ---- fs op ----------------------------------------------------------------------------------

// Fixed list of fs ops. Anything else is refused.
const (
	FSRead         = "read"
	FSWrite        = "write"
	FSEdit         = "edit"
	FSMultiEdit    = "multi_edit"
	FSNotebookEdit = "notebook_edit"
	FSGrep         = "grep"
	FSGlob         = "glob"
	FSLS           = "ls"
	FSStat         = "stat"
)

// FSOps lists every fs op.
var FSOps = []string{FSRead, FSWrite, FSEdit, FSMultiEdit, FSNotebookEdit, FSGrep, FSGlob, FSLS, FSStat}

// FSMutating reports whether an fs op changes files (guarded host fleets need confirm).
func FSMutating(op string) bool {
	switch op {
	case FSWrite, FSEdit, FSMultiEdit, FSNotebookEdit:
		return true
	}
	return false
}

// Values of the enum fields of FSOp.
const (
	OutputContent   = "content"
	OutputFiles     = "files_with_matches"
	OutputCount     = "count"
	CellCode        = "code"
	CellMarkdown    = "markdown"
	CellRaw         = "raw"
	NotebookReplace = "replace"
	NotebookInsert  = "insert"
	NotebookDelete  = "delete"
)

// FSEditItem is one replacement of a multi_edit.
type FSEditItem struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// FSOp is one file-tool call. Which fields are valid depends on Op (fsAllowed); a field set for
// an op that does not take it is refused. Paths may be absolute or relative to the session dir;
// the runner's OS permissions are the boundary, not this validator. For write, Length raw bytes
// with the given SHA256 follow as a body.
type FSOp struct {
	Op     string `json:"op"`
	Path   string `json:"path,omitempty"`
	Offset int64  `json:"offset,omitempty"` // read: first line (1-based; 0 = start)
	Limit  int    `json:"limit,omitempty"`  // read: lines

	Length int64  `json:"length,omitempty"` // write: body bytes
	SHA256 string `json:"sha256,omitempty"` // write: lowercase hex of the body
	Mode   uint32 `json:"mode,omitempty"`   // write: permission bits for a new file

	OldString  string       `json:"old_string,omitempty"`
	NewString  string       `json:"new_string,omitempty"`
	ReplaceAll bool         `json:"replace_all,omitempty"`
	Edits      []FSEditItem `json:"edits,omitempty"`

	CellID    string `json:"cell_id,omitempty"`
	NewSource string `json:"new_source,omitempty"`
	CellType  string `json:"cell_type,omitempty"`
	EditMode  string `json:"edit_mode,omitempty"`

	Pattern         string   `json:"pattern,omitempty"` // grep (RE2) and glob
	Glob            string   `json:"glob,omitempty"`
	Type            string   `json:"type,omitempty"`
	OutputMode      string   `json:"output_mode,omitempty"`
	CaseInsensitive bool     `json:"case_insensitive,omitempty"`
	LineNumbers     bool     `json:"line_numbers,omitempty"`
	After           int      `json:"after,omitempty"`
	Before          int      `json:"before,omitempty"`
	Context         int      `json:"context,omitempty"`
	Multiline       bool     `json:"multiline,omitempty"`
	HeadLimit       int      `json:"head_limit,omitempty"`
	Ignore          []string `json:"ignore,omitempty"` // ls
}

var fsAllowed = map[string]string{
	FSRead:         "path offset limit",
	FSWrite:        "path length sha256 mode",
	FSEdit:         "path old_string new_string replace_all",
	FSMultiEdit:    "path edits",
	FSNotebookEdit: "path cell_id new_source cell_type edit_mode",
	FSGrep:         "pattern path glob type output_mode case_insensitive line_numbers after before context multiline head_limit",
	FSGlob:         "pattern path",
	FSLS:           "path ignore",
	FSStat:         "path",
}

func (o *FSOp) setFields() []string {
	var s []string
	add := func(name string, set bool) {
		if set {
			s = append(s, name)
		}
	}
	add("path", o.Path != "")
	add("offset", o.Offset != 0)
	add("limit", o.Limit != 0)
	add("length", o.Length != 0)
	add("sha256", o.SHA256 != "")
	add("mode", o.Mode != 0)
	add("old_string", o.OldString != "")
	add("new_string", o.NewString != "")
	add("replace_all", o.ReplaceAll)
	add("edits", len(o.Edits) > 0)
	add("cell_id", o.CellID != "")
	add("new_source", o.NewSource != "")
	add("cell_type", o.CellType != "")
	add("edit_mode", o.EditMode != "")
	add("pattern", o.Pattern != "")
	add("glob", o.Glob != "")
	add("type", o.Type != "")
	add("output_mode", o.OutputMode != "")
	add("case_insensitive", o.CaseInsensitive)
	add("line_numbers", o.LineNumbers)
	add("after", o.After != 0)
	add("before", o.Before != 0)
	add("context", o.Context != 0)
	add("multiline", o.Multiline)
	add("head_limit", o.HeadLimit != 0)
	add("ignore", len(o.Ignore) > 0)
	return s
}

func allowedSet(list string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(list) {
		m[f] = true
	}
	return m
}

// Validate checks the op, refuses fields the op does not take, and bounds every value.
func (o *FSOp) Validate() error {
	list, ok := fsAllowed[o.Op]
	if !ok {
		return errors.New("unknown fs op")
	}
	allowed := allowedSet(list)
	for _, f := range o.setFields() {
		if !allowed[f] {
			return fmt.Errorf("field %s is not valid for fs op %s", f, o.Op)
		}
	}
	if err := checkText("path", o.Path, MaxPathBytes); err != nil {
		return err
	}
	needPath := o.Op != FSGrep && o.Op != FSGlob && o.Op != FSLS
	if needPath && o.Path == "" {
		return errors.New("path required")
	}
	switch o.Op {
	case FSRead:
		if o.Offset < 0 || o.Offset > 1<<31 {
			return errors.New("offset out of range")
		}
		if o.Limit < 0 || o.Limit > MaxLines {
			return errors.New("limit out of range")
		}
	case FSWrite:
		if o.Length < 0 || o.Length > MaxBodyBytes {
			return errors.New("length out of range")
		}
		if !ValidSHA256(o.SHA256) {
			return errors.New("sha256 must be 64 lowercase hex characters")
		}
		if o.Mode > maxFileMode {
			return errors.New("mode out of range")
		}
	case FSEdit:
		if err := checkEditPair(o.OldString, o.NewString); err != nil {
			return err
		}
	case FSMultiEdit:
		if len(o.Edits) == 0 || len(o.Edits) > MaxEdits {
			return fmt.Errorf("edits must hold 1-%d items", MaxEdits)
		}
		total := 0
		for _, e := range o.Edits {
			if err := checkEditPair(e.OldString, e.NewString); err != nil {
				return err
			}
			total += len(e.OldString) + len(e.NewString)
		}
		if total > MaxEditsBytes {
			return errors.New("edits too large")
		}
	case FSNotebookEdit:
		return o.validateNotebook()
	case FSGrep:
		return o.validateGrep()
	case FSGlob:
		if err := checkText("pattern", o.Pattern, MaxGlobBytes); err != nil {
			return err
		}
		if o.Pattern == "" {
			return errors.New("pattern required")
		}
	case FSLS:
		if len(o.Ignore) > MaxIgnoreGlobs {
			return errors.New("too many ignore globs")
		}
		for _, g := range o.Ignore {
			if g == "" {
				return errors.New("empty ignore glob")
			}
			if err := checkText("ignore glob", g, 256); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkEditPair(oldS, newS string) error {
	if oldS == "" {
		return errors.New("old_string required")
	}
	if oldS == newS {
		return errors.New("new_string must differ from old_string")
	}
	if err := checkText("old_string", oldS, MaxEditString); err != nil {
		return err
	}
	return checkText("new_string", newS, MaxEditString)
}

func (o *FSOp) validateNotebook() error {
	if err := checkText("new_source", o.NewSource, MaxEditString); err != nil {
		return err
	}
	if err := checkText("cell_id", o.CellID, 128); err != nil {
		return err
	}
	switch o.CellType {
	case "", CellCode, CellMarkdown, CellRaw:
	default:
		return errors.New("bad cell_type")
	}
	switch o.EditMode {
	case NotebookReplace:
		if o.CellID == "" {
			return errors.New("cell_id required for replace")
		}
	case NotebookInsert:
		if o.CellType == "" {
			return errors.New("cell_type required for insert")
		}
	case NotebookDelete:
		if o.CellID == "" {
			return errors.New("cell_id required for delete")
		}
		if o.NewSource != "" || o.CellType != "" {
			return errors.New("new_source and cell_type are not valid for delete")
		}
	default:
		return errors.New("edit_mode must be replace, insert or delete")
	}
	return nil
}

func (o *FSOp) validateGrep() error {
	if err := checkRE2("pattern", o.Pattern, MaxPatternBytes); err != nil {
		return err
	}
	if err := checkText("glob", o.Glob, MaxGlobBytes); err != nil {
		return err
	}
	if o.Type != "" && !typeRE.MatchString(o.Type) {
		return errors.New("bad type")
	}
	switch o.OutputMode {
	case "", OutputContent, OutputFiles, OutputCount:
	default:
		return errors.New("bad output_mode")
	}
	for _, c := range []struct {
		what string
		v    int
		max  int
	}{{"after", o.After, MaxContextLines}, {"before", o.Before, MaxContextLines},
		{"context", o.Context, MaxContextLines}, {"head_limit", o.HeadLimit, MaxLines}} {
		if c.v < 0 || c.v > c.max {
			return fmt.Errorf("%s out of range", c.what)
		}
	}
	return nil
}

// ---- job ops --------------------------------------------------------------------------------

// JobOp is one background-job call; the request Op (job_start, job_status, job_tail, job_cancel)
// decides which fields are valid. job_status without Job lists the session's jobs. job_tail
// returns the output produced since its previous call (the runner keeps the cursor).
type JobOp struct {
	Job         string `json:"job,omitempty"`
	Command     string `json:"command,omitempty"`
	Description string `json:"description,omitempty"`
	TimeoutS    int    `json:"timeout_s,omitempty"` // 0 = the runner's job_max
	Filter      string `json:"filter,omitempty"`    // tail: RE2, only matching lines
}

var jobAllowed = map[string]string{
	OpJobStart:  "command description timeout_s",
	OpJobStatus: "job",
	OpJobTail:   "job filter",
	OpJobCancel: "job",
}

// ValidateFor checks the op's fields; op is one of the job_* ops.
func (j *JobOp) ValidateFor(op string) error {
	list, ok := jobAllowed[op]
	if !ok {
		return errors.New("not a job op")
	}
	allowed := allowedSet(list)
	for name, set := range map[string]bool{"job": j.Job != "", "command": j.Command != "",
		"description": j.Description != "", "timeout_s": j.TimeoutS != 0, "filter": j.Filter != ""} {
		if set && !allowed[name] {
			return fmt.Errorf("field %s is not valid for %s", name, op)
		}
	}
	if j.Job != "" && !jobIDRE.MatchString(j.Job) {
		return errors.New("bad job id")
	}
	switch op {
	case OpJobStart:
		if j.Command == "" {
			return errors.New("command required")
		}
		if err := checkText("command", j.Command, MaxCommandBytes); err != nil {
			return err
		}
		if err := checkText("description", j.Description, MaxDescription); err != nil {
			return err
		}
		if j.TimeoutS < 0 || j.TimeoutS > MaxJobTimeoutS {
			return errors.New("timeout_s out of range")
		}
	case OpJobTail:
		if j.Job == "" {
			return errors.New("job required")
		}
		if j.Filter != "" {
			return checkRE2("filter", j.Filter, MaxPatternBytes)
		}
	case OpJobCancel:
		if j.Job == "" {
			return errors.New("job required")
		}
	}
	return nil
}

// OutputOp pages saved output: a foreground command's Ref, or a background Job's stream.
// Exactly one of Ref and Job. Offset/Limit are bytes; TailLines (the last N lines) excludes
// Offset; Grep keeps only matching lines.
type OutputOp struct {
	Ref       string `json:"ref,omitempty"`
	Job       string `json:"job,omitempty"`
	Stream    string `json:"stream,omitempty"` // "", "stdout" or "stderr" (job only)
	Offset    int64  `json:"offset,omitempty"`
	Limit     int64  `json:"limit,omitempty"` // 0 = MaxBodyBytes
	TailLines int    `json:"tail_lines,omitempty"`
	Grep      string `json:"grep,omitempty"`
}

// Validate bounds the fields.
func (o *OutputOp) Validate() error {
	if (o.Ref == "") == (o.Job == "") {
		return errors.New("exactly one of ref and job is required")
	}
	if o.Ref != "" && !refRE.MatchString(o.Ref) {
		return errors.New("bad ref")
	}
	if o.Job != "" && !jobIDRE.MatchString(o.Job) {
		return errors.New("bad job id")
	}
	switch o.Stream {
	case "":
	case "stdout", "stderr":
		if o.Job == "" {
			return errors.New("stream is only valid with job")
		}
	default:
		return errors.New("bad stream")
	}
	if o.Offset < 0 || o.Offset > 1<<62-1 {
		return errors.New("offset out of range")
	}
	if o.Limit < 0 || o.Limit > MaxBodyBytes {
		return errors.New("limit out of range")
	}
	if o.TailLines < 0 || o.TailLines > MaxLines {
		return errors.New("tail_lines out of range")
	}
	if o.TailLines != 0 && o.Offset != 0 {
		return errors.New("tail_lines and offset are exclusive")
	}
	if o.Grep != "" {
		return checkRE2("grep", o.Grep, MaxPatternBytes)
	}
	return nil
}

// ---- request validation for the dev ops -----------------------------------------------------

func (r *WorkspaceRequest) validateDev() error {
	if r.Spec != nil || len(r.Argv) > 0 || r.Cwd != "" || len(r.Env) > 0 || r.Path != "" ||
		r.Offset != 0 || r.Limit != 0 || r.Truncate || r.Mode != 0 || r.Length != 0 || r.TimeoutMs != 0 || r.Delete {
		return errors.New("legacy fields set on a dev op")
	}
	if r.ID != "" && !validWSID(r.ID) {
		return errors.New("bad id")
	}
	if r.StdinLen < 0 || r.StdinLen > MaxBodyBytes {
		return errors.New("stdin_len out of range")
	}
	n := 0
	for _, set := range []bool{r.FS != nil, r.Job != nil, r.Out != nil} {
		if set {
			n++
		}
	}
	if r.Op == OpCredsSync {
		if r.ID != "" || n != 0 {
			return errors.New("creds_sync takes no id or payload")
		}
		if r.StdinLen < 1 {
			return errors.New("creds_sync body required")
		}
		return nil
	}
	if r.ID == "" {
		return errors.New("id required")
	}
	if n != 1 {
		return errors.New("exactly one payload required")
	}
	bodyLen := int64(0)
	switch r.Op {
	case OpFS:
		if r.FS == nil {
			return errors.New("fs payload required")
		}
		if err := r.FS.Validate(); err != nil {
			return err
		}
		if r.FS.Op == FSWrite {
			bodyLen = r.FS.Length
		}
	case OpOutputRead:
		if r.Out == nil {
			return errors.New("out payload required")
		}
		if err := r.Out.Validate(); err != nil {
			return err
		}
	default: // job_*
		if r.Job == nil {
			return errors.New("job payload required")
		}
		if err := r.Job.ValidateFor(r.Op); err != nil {
			return err
		}
	}
	if r.StdinLen != bodyLen {
		return errors.New("stdin_len does not match the op's body")
	}
	return nil
}

// ---- results --------------------------------------------------------------------------------

// Kinds of an FSResult.
const (
	KindText     = "text"
	KindImage    = "image"
	KindBinary   = "binary"
	KindNotebook = "notebook"
	KindEntries  = "entries"
	KindNone     = "none"
)

// FSEntry is one ls, glob or stat entry.
type FSEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MTimeMs int64  `json:"mtime_ms"`
	Dir     bool   `json:"dir,omitempty"`
}

// FSResult is the outcome of one fs op. Length > 0: that many raw bytes (an image) follow the
// result frame. Truncated results always say how much was left out (never a silent cut).
type FSResult struct {
	Kind         string    `json:"kind"`
	Content      string    `json:"content,omitempty"`
	Size         int64     `json:"size,omitempty"`
	TotalLines   int       `json:"total_lines,omitempty"`
	Lines        int       `json:"lines,omitempty"`
	Count        int       `json:"count,omitempty"` // matches, files, replacements
	Truncated    bool      `json:"truncated,omitempty"`
	Omitted      int64     `json:"omitted,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	MIME         string    `json:"mime,omitempty"`
	Length       int64     `json:"length,omitempty"`
	Entries      []FSEntry `json:"entries,omitempty"`
	NewFile      bool      `json:"new_file,omitempty"`
	WrittenBytes int64     `json:"written_bytes,omitempty"`
}

func (f *FSResult) validate() error {
	switch f.Kind {
	case KindText, KindImage, KindBinary, KindNotebook, KindEntries, KindNone:
	default:
		return errors.New("bad fs result kind")
	}
	if len(f.Content) > MaxFSOutputText || len(f.Entries) > MaxListEntries {
		return errors.New("fs result too large")
	}
	for _, v := range []int64{f.Size, int64(f.TotalLines), int64(f.Lines), int64(f.Count), f.Omitted, f.WrittenBytes} {
		if v < 0 {
			return errors.New("negative fs result count")
		}
	}
	if f.Length < 0 || f.Length > MaxBodyBytes {
		return errors.New("bad fs result length")
	}
	if f.SHA256 != "" && !ValidSHA256(f.SHA256) {
		return errors.New("bad fs result sha256")
	}
	if len(f.MIME) > 128 {
		return errors.New("fs result mime too long")
	}
	return nil
}

// Job states.
const (
	JobRunning  = "running"
	JobExited   = "exited"
	JobCanceled = "canceled"
	JobTimedOut = "timed_out"
	JobLost     = "lost"
)

// JobResult describes one background job; Output carries new text for job_tail.
type JobResult struct {
	Job         string `json:"job"`
	State       string `json:"state"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	StartedMs   int64  `json:"started_ms"`
	EndedMs     int64  `json:"ended_ms,omitempty"`
	Description string `json:"description,omitempty"`
	StdoutBytes int64  `json:"stdout_bytes,omitempty"`
	StderrBytes int64  `json:"stderr_bytes,omitempty"`
	Output      string `json:"output,omitempty"`
	Omitted     int64  `json:"omitted,omitempty"`
}

func (j *JobResult) validate() error {
	if !jobIDRE.MatchString(j.Job) {
		return errors.New("bad job result id")
	}
	switch j.State {
	case JobRunning, JobExited, JobCanceled, JobTimedOut, JobLost:
	default:
		return errors.New("bad job state")
	}
	if len(j.Output) > MaxBodyBytes || len(j.Description) > MaxDescription*4 {
		return errors.New("job result too large")
	}
	for _, v := range []int64{j.StartedMs, j.EndedMs, j.StdoutBytes, j.StderrBytes, j.Omitted} {
		if v < 0 {
			return errors.New("negative job result count")
		}
	}
	return nil
}

// OutputResult is one page of saved output. Size is the total; EOF is set when the page reaches it.
type OutputResult struct {
	Content string `json:"content"`
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
	EOF     bool   `json:"eof,omitempty"`
	Lines   int    `json:"lines,omitempty"`
}

// CredsSyncResult acknowledges a creds_sync: the generation now held and how many credentials.
type CredsSyncResult struct {
	Generation int64 `json:"generation"`
	Count      int   `json:"count"`
}

func (r *WorkspaceResult) validateDevResult() error {
	if r.FS != nil {
		if err := r.FS.validate(); err != nil {
			return err
		}
	}
	if r.Job != nil {
		if err := r.Job.validate(); err != nil {
			return err
		}
	}
	if len(r.Jobs) > MaxJobList {
		return errors.New("too many jobs")
	}
	for i := range r.Jobs {
		if err := r.Jobs[i].validate(); err != nil {
			return err
		}
	}
	if o := r.Output; o != nil {
		if len(o.Content) > MaxBodyBytes || o.Offset < 0 || o.Size < 0 || o.Lines < 0 {
			return errors.New("bad output result")
		}
	}
	if c := r.Creds; c != nil && (c.Generation < 0 || c.Count < 0 || c.Count > MaxSyncCreds) {
		return errors.New("bad creds result")
	}
	return nil
}

// ---- credentials ----------------------------------------------------------------------------

// Credential kinds, HTTPS auth modes and SSH host binding values.
const (
	CredSSHKey     = "ssh-key"
	CredHTTPSToken = "https-token"
	CredRegistry   = "registry"

	AuthBasic  = "basic"
	AuthBearer = "bearer"
	AuthHeader = "header"

	BindRequired = "required"
	BindOff      = "off"
)

// Credential statuses reported by a runner.
const (
	CredOK       = "ok"
	CredExpiring = "expiring"
	CredExpired  = "expired"
	CredInvalid  = "invalid"
)

// ValidCredStatus reports whether s is a known credential status.
func ValidCredStatus(s string) bool {
	switch s {
	case CredOK, CredExpiring, CredExpired, CredInvalid:
		return true
	}
	return false
}

// ValidCredKind reports whether k is a known credential kind.
func ValidCredKind(k string) bool {
	switch k {
	case CredSSHKey, CredHTTPSToken, CredRegistry:
		return true
	}
	return false
}

// ValidCredName reports whether s is a valid credential name.
func ValidCredName(s string) bool { return credName.MatchString(s) }

// ValidateHosts checks a credential's host list: 1-16 unique lowercase host[:port] entries.
func ValidateHosts(hosts []string) error {
	if len(hosts) == 0 || len(hosts) > MaxCredHosts {
		return fmt.Errorf("hosts must hold 1-%d entries", MaxCredHosts)
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		name, port, hasPort := strings.Cut(h, ":")
		if len(h) > MaxHostBytes || !hostRE.MatchString(name) {
			return errors.New("bad host")
		}
		if hasPort {
			n := 0
			if port == "" || len(port) > 5 {
				return errors.New("bad host port")
			}
			for _, c := range port {
				if c < '0' || c > '9' {
					return errors.New("bad host port")
				}
				n = n*10 + int(c-'0')
			}
			if n < 1 || n > 65535 {
				return errors.New("bad host port")
			}
		}
		if seen[h] {
			return errors.New("duplicate host")
		}
		seen[h] = true
	}
	return nil
}

var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"proxy-authorization": true, "upgrade": true, "te": true, "trailer": true,
}

// CredMeta is the non-secret description of a credential, shared by credential.set and the
// runner-side creds_sync.
type CredMeta struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Hosts       []string `json:"hosts"`
	User        string   `json:"user,omitempty"`   // ssh user (default git), basic or registry user
	Auth        string   `json:"auth,omitempty"`   // https-token: basic | bearer | header
	Header      string   `json:"header,omitempty"` // https-token auth=header: the header name
	KnownHosts  []string `json:"known_hosts,omitempty"`
	HostBinding string   `json:"host_binding,omitempty"` // ssh-key: "" or required (default) | off
}

// Validate checks the shape per kind and refuses fields the kind does not take.
func (m *CredMeta) Validate() error {
	if !credName.MatchString(m.Name) {
		return errors.New("invalid credential name")
	}
	if !ValidCredKind(m.Kind) {
		return errors.New("invalid credential kind")
	}
	if err := ValidateHosts(m.Hosts); err != nil {
		return err
	}
	if m.User != "" && (len(m.User) > 128 || !printable(m.User)) {
		return errors.New("invalid user")
	}
	if len(m.KnownHosts) > MaxKnownHosts {
		return errors.New("too many known_hosts lines")
	}
	for _, l := range m.KnownHosts {
		if l == "" || len(l) > MaxKnownHostBytes || !printable(l) || strings.HasPrefix(l, "#") {
			return errors.New("invalid known_hosts line")
		}
	}
	switch m.Kind {
	case CredSSHKey:
		if m.User != "" && !sshUserRE.MatchString(m.User) {
			return errors.New("invalid ssh user")
		}
		if m.Auth != "" || m.Header != "" {
			return errors.New("auth and header are not valid for ssh-key")
		}
		if len(m.KnownHosts) == 0 {
			return errors.New("known_hosts required for ssh-key")
		}
		switch m.HostBinding {
		case "", BindRequired, BindOff:
		default:
			return errors.New("invalid host_binding")
		}
	case CredHTTPSToken:
		if len(m.KnownHosts) > 0 || m.HostBinding != "" {
			return errors.New("known_hosts and host_binding are only valid for ssh-key")
		}
		switch m.Auth {
		case AuthBasic:
			if m.User == "" || m.Header != "" {
				return errors.New("auth basic needs user and no header")
			}
		case AuthBearer:
			if m.User != "" || m.Header != "" {
				return errors.New("auth bearer takes no user or header")
			}
		case AuthHeader:
			if m.User != "" || !headerRE.MatchString(m.Header) || forbiddenHeaders[strings.ToLower(m.Header)] {
				return errors.New("auth header needs a valid header name and no user")
			}
		default:
			return errors.New("auth must be basic, bearer or header")
		}
	case CredRegistry:
		if len(m.KnownHosts) > 0 || m.HostBinding != "" || m.Auth != "" || m.Header != "" {
			return errors.New("registry takes only user and value")
		}
		if m.User == "" {
			return errors.New("user required for registry")
		}
	}
	return nil
}

// CredSecret is one credential as delivered to a runner: its description plus the secret Value.
// Value is held in memory only on the runner. The type redacts itself in fmt and slog output;
// never log it by any other route.
type CredSecret struct {
	CredMeta
	Version   int64  `json:"version"`
	ExpiresMs int64  `json:"expires_ms,omitempty"` // unix ms, 0 = none
	Value     string `json:"value"`
}

const redacted = "[credential redacted]"

// String implements fmt.Stringer without the value.
func (c CredSecret) String() string {
	return fmt.Sprintf("credential %q (%s) %s", c.Name, c.Kind, redacted)
}

// GoString implements fmt.GoStringer without the value.
func (c CredSecret) GoString() string { return c.String() }

// LogValue implements slog.LogValuer without the value.
func (c CredSecret) LogValue() slog.Value { return slog.StringValue(c.String()) }

// CredsSyncBody is the body of a creds_sync request: the runner's complete effective set. It
// replaces the previous set atomically; an empty set removes everything.
type CredsSyncBody struct {
	Generation  int64        `json:"generation"`
	Credentials []CredSecret `json:"credentials"`
}

// String implements fmt.Stringer without any value.
func (b CredsSyncBody) String() string {
	return fmt.Sprintf("creds_sync generation %d, %d credentials %s", b.Generation, len(b.Credentials), redacted)
}

// GoString implements fmt.GoStringer without any value.
func (b CredsSyncBody) GoString() string { return b.String() }

// LogValue implements slog.LogValuer without any value.
func (b CredsSyncBody) LogValue() slog.Value { return slog.StringValue(b.String()) }

// Validate bounds the set.
func (b *CredsSyncBody) Validate() error {
	if b.Generation < 0 {
		return errors.New("negative generation")
	}
	if len(b.Credentials) > MaxSyncCreds {
		return fmt.Errorf("at most %d credentials per sync", MaxSyncCreds)
	}
	names := map[string]bool{}
	for i := range b.Credentials {
		c := &b.Credentials[i]
		if err := c.CredMeta.Validate(); err != nil {
			return err
		}
		if names[c.Name] {
			return errors.New("duplicate credential name")
		}
		names[c.Name] = true
		if c.Version < 1 || c.ExpiresMs < 0 {
			return errors.New("bad credential version or expiry")
		}
		if c.Value == "" || len(c.Value) > MaxCredentialData || strings.IndexByte(c.Value, 0) >= 0 {
			return errors.New("bad credential value")
		}
	}
	return nil
}

// EncodeCredsSync validates b and returns the request body (at most MaxBodyBytes).
func EncodeCredsSync(b *CredsSyncBody) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxBodyBytes {
		return nil, fmt.Errorf("creds_sync body larger than %d bytes", MaxBodyBytes)
	}
	return out, nil
}

// DecodeCredsSync strictly decodes and validates a creds_sync body. Errors never contain values.
func DecodeCredsSync(data []byte) (*CredsSyncBody, error) {
	if len(data) > MaxBodyBytes {
		return nil, fmt.Errorf("creds_sync body larger than %d bytes", MaxBodyBytes)
	}
	var b CredsSyncBody
	if err := strictUnmarshal(data, &b); err != nil {
		return nil, errors.New("bad creds_sync body")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

// HeartbeatCredential is what a runner reports about a credential it holds: no value, no
// fingerprint. Status is one of ok, expiring, expired, invalid.
type HeartbeatCredential struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Hosts  []string `json:"hosts"`
	Status string   `json:"status"`
}

func (c *HeartbeatCredential) validate() error {
	if !credName.MatchString(c.Name) || !ValidCredKind(c.Kind) || !ValidCredStatus(c.Status) {
		return errors.New("heartbeat credential invalid")
	}
	return ValidateHosts(c.Hosts)
}

// Heartbeat feature names (Heartbeat.Features).
const (
	FeatureFS     = "fs"
	FeatureJobs   = "jobs"
	FeatureCreds  = "creds"
	FeatureEvents = "events"
	FeatureHost   = "host"
)

// MaxHeartbeatFeatures and MaxHeartbeatCredentials bound the M7 heartbeat fields.
const (
	MaxHeartbeatFeatures    = 16
	MaxHeartbeatCredentials = 64
	MaxHeartbeatBusy        = 1 << 16
)

func validFeature(f string) bool {
	switch f {
	case FeatureFS, FeatureJobs, FeatureCreds, FeatureEvents, FeatureHost:
		return true
	}
	return false
}

func (h *Heartbeat) validateDev() error {
	if len(h.Features) > MaxHeartbeatFeatures {
		return errors.New("too many heartbeat features")
	}
	seen := map[string]bool{}
	for _, f := range h.Features {
		if !validFeature(f) || seen[f] {
			return errors.New("heartbeat feature invalid")
		}
		seen[f] = true
	}
	if len(h.Credentials) > MaxHeartbeatCredentials {
		return errors.New("too many heartbeat credentials")
	}
	names := map[string]bool{}
	for i := range h.Credentials {
		if err := h.Credentials[i].validate(); err != nil {
			return err
		}
		if names[h.Credentials[i].Name] {
			return errors.New("duplicate heartbeat credential")
		}
		names[h.Credentials[i].Name] = true
	}
	if h.Busy < 0 || h.Busy > MaxHeartbeatBusy {
		return errors.New("heartbeat busy out of range")
	}
	return nil
}
