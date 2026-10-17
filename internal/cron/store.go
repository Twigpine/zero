package cron

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/fsutil"
)

const (
	StatusActive = "active"
	StatusPaused = "paused"

	// MaxRunHistory is the number of recent outcomes retained per job.
	MaxRunHistory = 1000
	maxRunBytes   = 1024 * 1024
)

// ErrJobNotFound is returned (wrapped) by Get when a job's metadata file is
// absent — a genuinely removed job. Callers use errors.Is to distinguish this
// from a transient read failure (IO error, permission), which must NOT be
// treated as "job removed".
var ErrJobNotFound = errors.New("cron job not found")

// Job is a stored scheduled job.
type Job struct {
	ID        string    `json:"id"`
	Expr      string    `json:"expr"`
	Prompt    string    `json:"prompt"`
	Cwd       string    `json:"cwd,omitempty"`
	Model     string    `json:"model,omitempty"`
	Status    string    `json:"status"`
	FireCount int       `json:"fireCount"`
	NextRunAt time.Time `json:"nextRunAt"`
	CreatedAt time.Time `json:"createdAt"`
}

// RunRecord is one fire's outcome, appended to the job's runs.jsonl.
type RunRecord struct {
	JobID        string    `json:"jobId"`
	At           time.Time `json:"at"`
	ExitCode     int       `json:"exitCode"`
	SessionTitle string    `json:"sessionTitle,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// StoreOptions configures a Store. RootDir defaults to DefaultRoot(os env); Now
// defaults to time.Now. Both injectable for tests.
type StoreOptions struct {
	RootDir string
	Now     func() time.Time
}

type Store struct {
	root string
	now  func() time.Time
	// replace overrides the run-history replacement primitive in tests; nil
	// uses the platform default.
	replace func(src, dst string) error
}

func NewStore(opts StoreOptions) *Store {
	root := opts.RootDir
	if root == "" {
		root = DefaultRoot(envMap())
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{root: root, now: now}
}

// DefaultRoot mirrors sessions.DefaultRoot: <XDG_DATA_HOME|~/.local/share>/zero/cron.
func DefaultRoot(env map[string]string) string {
	dataHome := strings.TrimSpace(env["XDG_DATA_HOME"])
	home := strings.TrimSpace(env["HOME"])
	if home == "" {
		// Mirror sessions.DefaultRoot: fall back to the OS user home so an unset
		// HOME (Windows, restricted shells) doesn't yield a RELATIVE ".local/share"
		// under the caller's cwd, which would scatter cron data per working dir.
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	base := dataHome
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "zero", "cron")
}

func envMap() map[string]string {
	return map[string]string{"XDG_DATA_HOME": os.Getenv("XDG_DATA_HOME"), "HOME": os.Getenv("HOME")}
}

// Add assigns an ID + CreatedAt and writes the job's metadata.json.
func (s *Store) Add(job Job) (Job, error) {
	if job.Status == "" {
		job.Status = StatusActive
	}
	job.CreatedAt = s.now().UTC()
	id, unlock, err := s.reserveID()
	if err != nil {
		return Job{}, err
	}
	// Held from reservation through initial persistence: without this, Remove
	// (which takes the same per-ID lock before deleting) could delete the
	// bare reservation directory the instant after Mkdir and report success,
	// then writeJob's MkdirAll would silently resurrect it and persist the
	// job anyway — or a second Add could re-reserve the freed id while this
	// writer is still running, putting two writers on the same metadata path.
	defer unlock()
	job.ID = id
	if err := s.writeReservedJob(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// reserveID atomically reserves an unused job id (by os.Mkdir-ing its
// directory) and returns the per-ID lock held for that id, still acquired.
// The caller must release it (via the returned unlock) only once the
// reservation has been committed (metadata.json written) or torn down —
// see Add.
func (s *Store) reserveID() (string, func(), error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return "", nil, fmt.Errorf("create cron store root: %w", err)
	}
	base := s.now().UTC().Format("20060102-150405")
	for n := 0; n < 100; n++ {
		id := base
		if n > 0 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		unlock, err := s.lockJob(id)
		if err != nil {
			return "", nil, err
		}
		mkErr := os.Mkdir(s.jobDir(id), 0o700)
		if mkErr == nil {
			return id, unlock, nil
		}
		unlock()
		if errors.Is(mkErr, os.ErrExist) {
			continue
		}
		return "", nil, fmt.Errorf("reserve cron job id %q: %w", id, mkErr)
	}
	return "", nil, errors.New("could not allocate a unique cron job id")
}

func (s *Store) jobDir(id string) string { return filepath.Join(s.root, id) }

// validID rejects ids that could escape the store root (path separators or
// traversal). reserveID-generated timestamp ids always pass; this guards
// externally-supplied ids (get/update/remove/append).
func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && filepath.Base(id) == id
}

func (s *Store) writeJob(job Job) error {
	dir := s.jobDir(job.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "metadata.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := fsutil.RenameWithRetry(tmp, filepath.Join(dir, "metadata.json"), nil); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) writeReservedJob(job Job) error {
	if err := s.writeJob(job); err != nil {
		if cleanupErr := os.RemoveAll(s.jobDir(job.ID)); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("remove failed cron job reservation: %w", cleanupErr))
		}
		return err
	}
	return nil
}

func (s *Store) Get(id string) (Job, error) {
	if !validID(id) {
		return Job{}, fmt.Errorf("invalid cron job id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(s.jobDir(id), "metadata.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Job{}, fmt.Errorf("cron job %q: %w", id, ErrJobNotFound)
		}
		// A transient read failure (IO error, permission) is NOT a missing job;
		// surface it as a distinct error so callers don't mistake it for removal.
		return Job{}, fmt.Errorf("read cron job %q: %w", id, err)
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, fmt.Errorf("cron job %q is corrupt: %w", id, err)
	}
	return job, nil
}

func (s *Store) Update(job Job) error {
	if !validID(job.ID) {
		return fmt.Errorf("invalid cron job id %q", job.ID)
	}
	unlock, err := s.lockJob(job.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(s.jobDir(job.ID)); err != nil {
		return fmt.Errorf("cron job %q not found", job.ID)
	}
	return s.writeJob(job)
}

// Mutate atomically updates job id under the per-job cross-process lock, closing
// the read-modify-write race between concurrent schedulers (and against Update/
// Remove). It re-reads the CURRENT on-disk job and passes it to mutate along with
// any read error; mutate returns the job to persist. A removed job aborts with
// ErrJobNotFound (no recreate). A transient read error (not removal) is surfaced
// via readErr; the callback can return it to abort without overwriting state.
func (s *Store) Mutate(id string, mutate func(current Job, readErr error) (Job, error)) (Job, error) {
	if !validID(id) {
		return Job{}, fmt.Errorf("invalid cron job id %q", id)
	}
	unlock, err := s.lockJob(id)
	if err != nil {
		return Job{}, err
	}
	defer unlock()
	current, readErr := s.Get(id)
	if errors.Is(readErr, ErrJobNotFound) {
		return Job{}, ErrJobNotFound
	}
	next, err := mutate(current, readErr)
	if err != nil {
		return Job{}, err
	}
	if err := s.writeJob(next); err != nil {
		return Job{}, err
	}
	return next, nil
}

func (s *Store) Remove(id string) error {
	if !validID(id) {
		return fmt.Errorf("invalid cron job id %q", id)
	}
	unlock, err := s.lockJob(id)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(s.jobDir(id)); err != nil {
		return fmt.Errorf("cron job %q not found", id)
	}
	return os.RemoveAll(s.jobDir(id))
}

func (s *Store) List() ([]Job, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []Job
	var corrupt []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(s.jobDir(e.Name()), "metadata.json")); statErr != nil {
			continue // a directory without metadata.json is not a job
		}
		job, gerr := s.Get(e.Name())
		if gerr != nil {
			corrupt = append(corrupt, e.Name())
			continue
		}
		jobs = append(jobs, job)
	}
	// Surface corrupt jobs (jobs slice is still authoritative; callers treat this
	// as a warning, not a fatal error, so one bad job never hides the rest).
	if len(corrupt) > 0 {
		return jobs, fmt.Errorf("skipped %d unreadable cron job(s): %s", len(corrupt), strings.Join(corrupt, ", "))
	}
	return jobs, nil
}

func (s *Store) AppendRun(id string, rec RunRecord) error {
	if !validID(id) {
		return fmt.Errorf("invalid cron job id %q", id)
	}
	unlock, err := s.lockJob(id)
	if err != nil {
		return err
	}
	defer unlock()
	// Do not resurrect a job removed mid-run. The lock serializes this check
	// and publication against Remove and other history writers/readers.
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "metadata.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	dir := s.jobDir(id)
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(line) >= maxRunBytes {
		return fmt.Errorf("cron run record exceeds %d bytes", maxRunBytes-1)
	}
	runs, err := s.readRuns(id, MaxRunHistory-1)
	if err != nil {
		return err
	}
	// Publish a complete snapshot even below the retention boundary so readers
	// never see a partial append. Legacy oversized logs compact on their next run.
	f, err := os.CreateTemp(dir, "runs-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	w := bufio.NewWriter(f)
	for _, run := range runs {
		if err := json.NewEncoder(w).Encode(run); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	// Surface a buffered-write failure that only materializes on Close.
	if err := f.Close(); err != nil {
		return err
	}
	path := filepath.Join(dir, "runs.jsonl")
	// ReplaceWithRetry keeps a DACL applied to runs.jsonl itself; a rename would
	// publish the temporary file's inherited directory DACL instead.
	err = fsutil.ReplaceWithRetry(f.Name(), path, s.replace)
	var committed *fsutil.CommittedReplacementCleanupError
	if err == nil || errors.As(err, &committed) || !fsutil.IsSharingOrLockViolation(err) {
		return err
	}
	// Windows refuses to replace a log another process holds open. Append the
	// record instead so the outcome survives; compaction resumes on a later run.
	// Other failures are returned so a log that can never be replaced cannot
	// grow past the retention limit. The leading newline ends any unterminated
	// tail, and readRuns skips the blank line it may leave. Zero's own readers
	// hold the job lock.
	if appendErr := appendRunLine(path, line); appendErr != nil {
		return errors.Join(err, appendErr)
	}
	return nil
}

func appendRunLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	record := append(append([]byte{'\n'}, line...), '\n')
	if _, err := f.Write(record); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Runs returns the newest limit valid records in append order (oldest first).
// Non-positive limits and limits above MaxRunHistory use MaxRunHistory. It reads
// backwards from the tail, including for legacy logs not yet compacted.
func (s *Store) Runs(id string, limit int) ([]RunRecord, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid cron job id %q", id)
	}
	if limit <= 0 || limit > MaxRunHistory {
		limit = MaxRunHistory
	}
	unlock, err := s.lockJob(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.readRuns(id, limit)
}

// readRuns requires the job lock. Memory is bounded by limit records plus one
// line. Malformed and oversized legacy lines are skipped so they cannot prevent
// recording new outcomes during compaction.
func (s *Store) readRuns(id string, limit int) ([]RunRecord, error) {
	f, err := os.Open(filepath.Join(s.jobDir(id), "runs.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var runs []RunRecord
	var line []byte
	oversized := false
	decode := func() {
		slices.Reverse(line)
		var rec RunRecord
		if json.Unmarshal(line, &rec) == nil {
			runs = append(runs, rec)
		}
		line = line[:0]
	}
	var block [4096]byte
	for end := info.Size(); end > 0 && len(runs) < limit; {
		start := max(int64(0), end-int64(len(block)))
		n, err := f.ReadAt(block[:end-start], start)
		if err != nil {
			return nil, err
		}
		for i := n - 1; i >= 0 && len(runs) < limit; i-- {
			if block[i] == '\n' {
				if !oversized {
					decode()
				}
				oversized = false
			} else if !oversized {
				if len(line) >= maxRunBytes-1 {
					line = line[:0]
					oversized = true
					continue
				}
				line = append(line, block[i])
			}
		}
		end = start
	}
	if len(runs) < limit && len(line) > 0 {
		decode()
	}
	slices.Reverse(runs)
	return runs, nil
}
