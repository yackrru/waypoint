package state

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/hashicorp/go-memdb"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/hashicorp/waypoint/internal/server/gen"
	"github.com/hashicorp/waypoint/internal/server/logbuffer"
)

var (
	jobBucket = []byte("jobs")

	jobWaitingTimeout   = 2 * time.Minute
	jobHeartbeatTimeout = 2 * time.Minute
)

const (
	jobTableName            = "jobs"
	jobIdIndexName          = "id"
	jobStateIndexName       = "state"
	jobQueueTimeIndexName   = "queue-time"
	jobTargetIdIndexName    = "target-id"
	jobSingletonIdIndexName = "singleton-id"

	maximumJobsIndexed = 10000
)

func init() {
	dbBuckets = append(dbBuckets, jobBucket)
	dbIndexers = append(dbIndexers, (*State).jobIndexInit)
	schemas = append(schemas, jobSchema)
}

func jobSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: jobTableName,
		Indexes: map[string]*memdb.IndexSchema{
			jobIdIndexName: {
				Name:         jobIdIndexName,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "Id",
				},
			},

			jobStateIndexName: {
				Name:         jobStateIndexName,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.IntFieldIndex{
					Field: "State",
				},
			},

			jobQueueTimeIndexName: {
				Name:         jobQueueTimeIndexName,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.IntFieldIndex{
							Field: "State",
						},

						&IndexTime{
							Field: "QueueTime",
							Asc:   true,
						},
					},
				},
			},

			jobTargetIdIndexName: {
				Name:         jobTargetIdIndexName,
				AllowMissing: true,
				Unique:       true,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.IntFieldIndex{
							Field: "State",
						},

						&memdb.StringFieldIndex{
							Field:     "TargetRunnerId",
							Lowercase: true,
						},

						&IndexTime{
							Field: "QueueTime",
							Asc:   true,
						},
					},
				},
			},

			jobSingletonIdIndexName: {
				Name:         jobSingletonIdIndexName,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.CompoundIndex{
					Indexes: []memdb.Indexer{
						&memdb.StringFieldIndex{
							Field:     "SingletonId",
							Lowercase: true,
						},

						&memdb.IntFieldIndex{
							Field: "State",
						},
					},
				},
			},
		},
	}
}

type jobIndex struct {
	Id string

	// SingletonId matches singleton_id if set on the job.
	SingletonId string

	// OpType is the operation type for the job.
	OpType reflect.Type

	// The project/workspace that this job is part of. This is used
	// to determine if the job is blocked. See job_assigned.go for more details.
	Application *pb.Ref_Application
	Workspace   *pb.Ref_Workspace

	// QueueTime is the time that the job was queued.
	QueueTime time.Time

	// TargetAny will be true if this job targets anything
	TargetAny bool

	// TargetRunnerId is the ID of the runner to target.
	TargetRunnerId string

	// State is the current state of this job.
	State pb.Job_State

	// StateTimer holds a timer that is usually acting as a timeout mechanism
	// on the current state. When the state changes, the timer should be cancelled.
	StateTimer *time.Timer

	// OutputBuffer stores the terminal output
	OutputBuffer *logbuffer.Buffer
}

// A helper, pulled out rather than on a value to allow it to be used against
// pb.Job,s and jobIndex's alike.
func jobIsCompleted(state pb.Job_State) bool {
	switch state {
	case pb.Job_ERROR, pb.Job_SUCCESS:
		return true
	default:
		return false
	}
}

// Job is the exported structure that is returned for most state APIs
// and gives callers access to more information than the pure job structure.
type Job struct {
	// Full job structure.
	*pb.Job

	// OutputBuffer is the terminal output for this job. This is a buffer
	// that may not contain the full amount of output depending on the
	// time of connection.
	OutputBuffer *logbuffer.Buffer

	// Blocked is true if this job is blocked on another job for the same
	// project/app/workspace.
	Blocked bool
}

// JobCreate queues the given jobs. If any job fails to queue, no jobs
// are queued. If partial failures are acceptible, call this multiple times
// with a single job.
func (s *State) JobCreate(jobs ...*pb.Job) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	err := s.db.Update(func(dbTxn *bolt.Tx) error {
		// Go through each job one at a time. If any fail, the transaction
		// is aborted so the whole thing will fail.
		for _, job := range jobs {
			if err := s.jobCreate(dbTxn, txn, job); err != nil {
				return err
			}
		}

		return nil
	})
	if err == nil {
		txn.Commit()
	}

	return err
}

// JobList returns the list of jobs.
func (s *State) JobList() ([]*pb.Job, error) {
	memTxn := s.inmem.Txn(false)
	defer memTxn.Abort()

	iter, err := memTxn.Get(jobTableName, jobIdIndexName+"_prefix", "")
	if err != nil {
		return nil, err
	}

	var result []*pb.Job
	for {
		next := iter.Next()
		if next == nil {
			break
		}
		idx := next.(*jobIndex)

		var job *pb.Job
		err = s.db.View(func(dbTxn *bolt.Tx) error {
			job, err = s.jobById(dbTxn, idx.Id)
			return err
		})

		result = append(result, job)
	}

	return result, nil
}

// JobById looks up a job by ID. The returned Job will be a deep copy
// of the job so it is safe to read/write. If the job can't be found,
// a nil result with no error is returned.
func (s *State) JobById(id string, ws memdb.WatchSet) (*Job, error) {
	memTxn := s.inmem.Txn(false)
	defer memTxn.Abort()

	watchCh, raw, err := memTxn.FirstWatch(jobTableName, jobIdIndexName, id)
	if err != nil {
		return nil, err
	}

	ws.Add(watchCh)

	if raw == nil {
		return nil, nil
	}
	jobIdx := raw.(*jobIndex)

	// Get blocked status if it is queued.
	var blocked bool
	if jobIdx.State == pb.Job_QUEUED {
		blocked, err = s.jobIsBlocked(memTxn, jobIdx, ws)
		if err != nil {
			return nil, err
		}
	}

	var job *pb.Job
	err = s.db.View(func(dbTxn *bolt.Tx) error {
		job, err = s.jobById(dbTxn, jobIdx.Id)
		return err
	})

	result := jobIdx.Job(job)
	result.Blocked = blocked

	return result, err
}

// JobPeekForRunner effectively simulates JobAssignForRunner with two changes:
// (1) jobs are not actually assigned (they remain queued) and (2) this will
// not block if a job isn't available. If a job isn't available, this will
// return (nil, nil).
func (s *State) JobPeekForRunner(ctx context.Context, r *pb.Runner) (*Job, error) {
	// The false,false here will (1) not block and (2) not assign
	return s.jobAssignForRunner(ctx, r, false, false)
}

// JobAssignForRunner will wait for and assign a job to a specific runner.
// This will automatically evaluate any conditions that the runner and/or
// job may have on assignability.
//
// The assigned job is put into a "waiting" state until the runner
// acks the assignment which can be set with JobAck.
//
// If ctx is provided and assignment has to block waiting for new jobs,
// this will cancel when the context is done.
func (s *State) JobAssignForRunner(ctx context.Context, r *pb.Runner) (*Job, error) {
	return s.jobAssignForRunner(ctx, r, true, true)
}

func (s *State) jobAssignForRunner(ctx context.Context, r *pb.Runner, block, assign bool) (*Job, error) {
	var txn *memdb.Txn

RETRY_ASSIGN:
	// If our transaction is not nil that means this is a repeated time around.
	// If we aren't blocking, return now.
	if txn != nil && !block {
		return nil, nil
	}

	txn = s.inmem.Txn(false)
	defer txn.Abort()

	// Turn our runner into a runner record so we can more efficiently assign
	runnerRec := newRunnerRecord(r)

	// candidateQuery finds candidate jobs to assign.
	type candidateFunc func(*memdb.Txn, memdb.WatchSet, *runnerRecord) (*jobIndex, error)
	candidateQuery := []candidateFunc{
		s.jobCandidateById,
		s.jobCandidateAny,
	}

	// If the runner is by id only, then explicitly set it to by id only.
	// We explicitly set the full list so that if we add more candidate
	// searches in the future, we're unlikely to break this.
	if r.ByIdOnly {
		candidateQuery = []candidateFunc{s.jobCandidateById}
	}

	// Build the list of candidates
	var candidates []*jobIndex
	ws := memdb.NewWatchSet()
	for _, f := range candidateQuery {
		job, err := f(txn, ws, runnerRec)
		if err != nil {
			return nil, err
		}
		if job == nil {
			continue
		}

		candidates = append(candidates, job)
	}

	// If we have no candidates, then we have to wait for a job to show up.
	// We set up a blocking query on the job table for a non-assigned job.
	if len(candidates) == 0 {
		iter, err := txn.Get(jobTableName, jobStateIndexName, pb.Job_QUEUED)
		if err != nil {
			return nil, err
		}

		ws.Add(iter.WatchCh())
	}

	// We're done reading so abort the transaction
	txn.Abort()

	// If we have a watch channel set that means we didn't find any
	// results and we need to retry after waiting for changes.
	if len(candidates) == 0 {
		if block {
			ws.WatchCtx(ctx)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		goto RETRY_ASSIGN
	}

	// We sort our candidates by queue time so that we can find the earliest
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].QueueTime.Before(candidates[j].QueueTime)
	})

	// Grab a write lock since we're going to delete, modify, add the
	// job that we chose. No need to defer here since the first defer works
	// at the top of the func.
	//
	// Write locks are exclusive so this will ensure we're the only one
	// writing at a time. This lets us be sure we're the only one "assigning"
	// a job candidate.
	//
	// Note: we only grab a write lock if we're assigning. If we're not
	// assigning then we grab a read lock.
	txn = s.inmem.Txn(assign)
	for _, job := range candidates {
		// Get the job
		raw, err := txn.First(jobTableName, jobIdIndexName, job.Id)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			// The job no longer exists. It may be canceled or something.
			// Invalid candidate, continue to next.
			continue
		}

		// We need to verify that in the time between our candidate search
		// and our write lock acquisition, that this job hasn't been assigned,
		// canceled, etc. If so, this is an invalid candidate.
		job := raw.(*jobIndex)
		if job == nil || job.State != pb.Job_QUEUED {
			continue
		}

		// We also need to recheck that we aren't blocked. If we're blocked
		// now then we need to skip this job.
		if blocked, err := s.jobIsBlocked(txn, job, nil); blocked {
			continue
		} else if err != nil {
			return nil, err
		}

		// If we've been requested to not assign, then we found our result.
		if !assign {
			// We're no longer going to use the memdb txn
			txn.Abort()

			var pbjob *pb.Job
			err = s.db.View(func(dbTxn *bolt.Tx) error {
				pbjob, err = s.jobById(dbTxn, job.Id)
				return err
			})
			if err != nil {
				return nil, err
			}

			return job.Job(pbjob), nil
		}

		// We're now modifying this job, so perform a copy
		job = job.Copy()

		// Update our state and update our on-disk job
		job.State = pb.Job_WAITING
		result, err := s.jobReadAndUpdate(job.Id, func(jobpb *pb.Job) error {
			jobpb.State = job.State
			jobpb.AssignTime, err = ptypes.TimestampProto(time.Now())
			if err != nil {
				// This should never happen since encoding a time now should be safe
				panic("time encoding failed: " + err.Error())
			}

			return nil
		})
		if err != nil {
			return nil, err
		}

		// Create our timer to requeue this if it isn't acked
		job.StateTimer = time.AfterFunc(jobWaitingTimeout, func() {
			s.log.Info("job ack timer expired", "job", job.Id, "timeout", jobWaitingTimeout)
			s.JobAck(job.Id, false)
		})

		if err := txn.Insert(jobTableName, job); err != nil {
			return nil, err
		}

		// Update our assignment state
		if err := s.jobAssignedSet(txn, job, true); err != nil {
			s.JobAck(job.Id, false)
			return nil, err
		}

		txn.Commit()
		return job.Job(result), nil
	}
	txn.Abort()

	// If we reached here, all of our candidates were invalid, we retry
	goto RETRY_ASSIGN
}

// JobAck acknowledges that a job has been accepted or rejected by the runner.
// If ack is false, then this will move the job back to the queued state
// and be eligible for assignment.
func (s *State) JobAck(id string, ack bool) (*Job, error) {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	// If the job is not in the assigned state, then this is an error.
	if job.State != pb.Job_WAITING {
		return nil, status.Errorf(codes.FailedPrecondition,
			"job can't be acked from state: %s",
			job.State.String())
	}

	// We're now modifying this job, so perform a copy
	job = job.Copy()

	result, err := s.jobReadAndUpdate(job.Id, func(jobpb *pb.Job) error {
		if ack {
			// Set to accepted
			job.State = pb.Job_RUNNING
			jobpb.State = job.State
			jobpb.AckTime, err = ptypes.TimestampProto(time.Now())
			if err != nil {
				// This should never happen since encoding a time now should be safe
				panic("time encoding failed: " + err.Error())
			}

			// We also initialize the output buffer here because we can
			// expect output to begin streaming in.
			job.OutputBuffer = logbuffer.New()
		} else {
			// Set to queued
			job.State = pb.Job_QUEUED
			jobpb.State = job.State
			jobpb.AssignTime = nil
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Cancel our timer
	if job.StateTimer != nil {
		job.StateTimer.Stop()
		job.StateTimer = nil
	}

	// Create a new timer that we'll use for our heartbeat. After this
	// timer expires, the job will immediately move to an error state.
	job.StateTimer = time.AfterFunc(jobHeartbeatTimeout, func() {
		s.log.Info("canceling job due to heartbeat timeout", "job", job.Id)
		// Force cancel
		err := s.JobCancel(job.Id, true)
		if err != nil {
			s.log.Error("error canceling job due to heartbeat failure", "error", err, "job", job.Id)
		}
	})

	s.log.Debug("heartbeat timer set", "job", job.Id, "timeout", jobHeartbeatTimeout)

	// Insert to update
	if err := txn.Insert(jobTableName, job); err != nil {
		return nil, err
	}

	// Update our assigned state if we nacked
	if !ack {
		if err := s.jobAssignedSet(txn, job, false); err != nil {
			return nil, err
		}
	}

	txn.Commit()
	return job.Job(result), nil
}

// JobUpdateRef sets the data_source_ref field for a job. This job can be
// in any state.
func (s *State) JobUpdateRef(id string, ref *pb.Job_DataSource_Ref) error {
	return s.JobUpdate(id, func(jobpb *pb.Job) error {
		jobpb.DataSourceRef = ref
		return nil
	})
}

// JobUpdate calls the given callback to update fields on the job data.
// The callback is called in the context of a database write lock so it
// should NOT compute anything and should be fast. The callback can return
// an error to abort the transaction.
func (s *State) JobUpdate(id string, cb func(jobpb *pb.Job) error) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return err
	}
	if raw == nil {
		return status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	_, err = s.jobReadAndUpdate(job.Id, func(jobpb *pb.Job) error {
		return cb(jobpb)
	})
	if err != nil {
		return err
	}

	// Insert to update
	if err := txn.Insert(jobTableName, job); err != nil {
		return err
	}

	txn.Commit()
	return nil
}

// JobComplete marks a running job as complete. If an error is given,
// the job is marked as failed (a completed state). If no error is given,
// the job is marked as successful.
func (s *State) JobComplete(id string, result *pb.Job_Result, cerr error) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return err
	}
	if raw == nil {
		return status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	// Update our assigned state
	if err := s.jobAssignedSet(txn, job, false); err != nil {
		return err
	}

	// If the job is not in the assigned state, then this is an error.
	if job.State != pb.Job_RUNNING {
		return status.Errorf(codes.FailedPrecondition,
			"job can't be completed from state: %s",
			job.State.String())
	}

	// We're now modifying this job, so perform a copy
	job = job.Copy()

	_, err = s.jobReadAndUpdate(job.Id, func(jobpb *pb.Job) error {
		// Set to complete, assume success for now
		job.State = pb.Job_SUCCESS
		jobpb.State = job.State
		jobpb.Result = result
		jobpb.CompleteTime, err = ptypes.TimestampProto(time.Now())
		if err != nil {
			// This should never happen since encoding a time now should be safe
			panic("time encoding failed: " + err.Error())
		}

		if cerr != nil {
			job.State = pb.Job_ERROR
			jobpb.State = job.State

			st, _ := status.FromError(cerr)
			jobpb.Error = st.Proto()
		}

		return nil
	})
	if err != nil {
		return err
	}

	// End the job
	job.End()

	// Insert to update
	if err := txn.Insert(jobTableName, job); err != nil {
		return err
	}

	txn.Commit()
	return nil
}

// JobCancel marks a job as cancelled. This will set the internal state
// and request the cancel but if the job is running then it is up to downstream
// to listen for and react to Job changes for cancellation.
func (s *State) JobCancel(id string, force bool) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return err
	}
	if raw == nil {
		return status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	if err := s.jobCancel(txn, job, force); err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *State) jobCancel(txn *memdb.Txn, job *jobIndex, force bool) error {
	job = job.Copy()
	oldState := job.State

	// How we handle cancel depends on the state
	switch job.State {
	case pb.Job_ERROR, pb.Job_SUCCESS:
		s.log.Debug("attempted to cancel completed job", "state", job.State.String(), "job", job.Id)
		// Jobs that are already completed do nothing for cancellation.
		// We do not mark that they were requested as cancelled since they
		// completed fine.
		return nil

	case pb.Job_QUEUED:
		// For queued jobs, we immediately transition them to an error state.
		job.State = pb.Job_ERROR

	case pb.Job_WAITING, pb.Job_RUNNING:
		// For these states, we just need to mark it as cancelled and have
		// downstream listeners complete the job. However, if we are forcing
		// then we immediately transition to error.
		if force {
			job.State = pb.Job_ERROR
			job.End()
		}
	}

	s.log.Debug("changing job state for cancel", "old-state", oldState.String(), "new-state", job.State.String(), "job", job.Id, "force", force)

	if force && job.State == pb.Job_ERROR {
		// Update our assigned state to unblock future jobs
		if err := s.jobAssignedSet(txn, job, false); err != nil {
			return err
		}
	}

	// Persist the on-disk data
	_, err := s.jobReadAndUpdate(job.Id, func(jobpb *pb.Job) error {
		var err error
		jobpb.State = job.State
		jobpb.CancelTime, err = ptypes.TimestampProto(time.Now())
		if err != nil {
			// This should never happen since encoding a time now should be safe
			panic("time encoding failed: " + err.Error())
		}

		// If we transitioned to the error state we note that we were force
		// cancelled. We can only be in the error state under that scenario
		// since otherwise we would've returned early.
		if jobpb.State == pb.Job_ERROR {
			jobpb.Error = status.New(codes.Canceled, "canceled").Proto()
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Store the inmem data
	// This will be seen by a currently running RunnerJobStream goroutine, which
	// will then see that the job has been canceled and send the request to cancel
	// down to the runner.
	if err := txn.Insert(jobTableName, job); err != nil {
		return err
	}

	return nil
}

// JobHeartbeat resets the heartbeat timer for a running job. If the job
// is not currently running this does nothing, it will not return an error.
// If the job doesn't exist then this will return an error.
func (s *State) JobHeartbeat(id string) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	if err := s.jobHeartbeat(txn, id); err != nil {
		return err
	}

	txn.Commit()
	return nil
}

func (s *State) jobHeartbeat(txn *memdb.Txn, id string) error {
	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return err
	}
	if raw == nil {
		return status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	// If the job is not in the running state, we do nothing.
	if job.State != pb.Job_RUNNING {
		return nil
	}

	// If the state timer is nil... that is weird but we ignore it here.
	// It is up to other parts of the job system to ensure a running
	// job has a heartbeat timer.
	if job.StateTimer == nil {
		s.log.Info("job with no start timer detected", "job", id)
		return nil
	}

	// Reset the timer
	job.StateTimer.Reset(jobHeartbeatTimeout)

	return nil
}

// JobExpire expires a job. This will cancel the job if it is still queued.
func (s *State) JobExpire(id string) error {
	txn := s.inmem.Txn(true)
	defer txn.Abort()

	// Get the job
	raw, err := txn.First(jobTableName, jobIdIndexName, id)
	if err != nil {
		return err
	}
	if raw == nil {
		return status.Errorf(codes.NotFound, "job not found: %s", id)
	}
	job := raw.(*jobIndex)

	// How we handle depends on the state
	switch job.State {
	case pb.Job_QUEUED, pb.Job_WAITING:
		if err := s.jobCancel(txn, job, false); err != nil {
			return err
		}

	default:
	}

	txn.Commit()
	return nil
}

// JobIsAssignable returns whether there is a registered runner that
// meets the requirements to run this job.
//
// If this returns true, the job if queued should eventually be assigned
// successfully to a runner. An assignable result does NOT mean that it will be
// in queue a short amount of time.
//
// Note the result is a point-in-time result. If the only candidate runners
// deregister between this returning true and queueing, the job may still
// sit in a queue indefinitely.
func (s *State) JobIsAssignable(ctx context.Context, jobpb *pb.Job) (bool, error) {
	memTxn := s.inmem.Txn(false)
	defer memTxn.Abort()

	// If we have no runners, we cannot be assigned
	empty, err := s.runnerEmpty(memTxn)
	if err != nil {
		return false, err
	}
	if empty {
		return false, nil
	}

	// If we have a special targeting constraint, that has to be met
	var iter memdb.ResultIterator
	var targetCheck func(*pb.Runner) (bool, error)
	switch v := jobpb.TargetRunner.Target.(type) {
	case *pb.Ref_Runner_Any:
		// We need a special target check that disallows by ID only
		targetCheck = func(r *pb.Runner) (bool, error) {
			return !r.ByIdOnly, nil
		}

		iter, err = memTxn.LowerBound(runnerTableName, runnerIdIndexName, "")

	case *pb.Ref_Runner_Id:
		iter, err = memTxn.Get(runnerTableName, runnerIdIndexName, v.Id.Id)

	default:
		return false, fmt.Errorf("unknown runner target value: %#v", jobpb.TargetRunner.Target)
	}
	if err != nil {
		return false, err
	}

	for {
		raw := iter.Next()
		if raw == nil {
			// We're out of candidates and we found none.
			return false, nil
		}
		runner := raw.(*runnerRecord)

		// Check our target-specific check
		if targetCheck != nil {
			check, err := targetCheck(runner.Runner)
			if err != nil {
				return false, err
			}
			if !check {
				continue
			}
		}

		// This works!
		return true, nil
	}
}

// jobIndexInit initializes the config index from persisted data.
func (s *State) jobIndexInit(dbTxn *bolt.Tx, memTxn *memdb.Txn) error {
	bucket := dbTxn.Bucket(jobBucket)
	c := bucket.Cursor()

	var cnt int

	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		var value pb.Job
		if err := proto.Unmarshal(v, &value); err != nil {
			return err
		}

		// if we still have headroom for more indexed jobs OR the job hasn't finished yet,
		// index it.
		if cnt < maximumJobsIndexed || !jobIsCompleted(value.State) {
			cnt++
			idx, err := s.jobIndexSet(memTxn, k, &value)
			if err != nil {
				return err
			}

			// If the job was running or waiting, set it as assigned.
			if value.State == pb.Job_RUNNING || value.State == pb.Job_WAITING {
				if err := s.jobAssignedSet(memTxn, idx, true); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// jobIndexSet writes an index record for a single job.
func (s *State) jobIndexSet(txn *memdb.Txn, id []byte, jobpb *pb.Job) (*jobIndex, error) {
	rec := &jobIndex{
		Id:          jobpb.Id,
		SingletonId: jobpb.SingletonId,
		State:       jobpb.State,
		Application: jobpb.Application,
		Workspace:   jobpb.Workspace,
		OpType:      reflect.TypeOf(jobpb.Operation),
	}

	// Target
	if jobpb.TargetRunner == nil {
		return nil, fmt.Errorf("job target runner must be set")
	}
	switch v := jobpb.TargetRunner.Target.(type) {
	case *pb.Ref_Runner_Any:
		rec.TargetAny = true

	case *pb.Ref_Runner_Id:
		rec.TargetRunnerId = v.Id.Id

	default:
		return nil, fmt.Errorf("unknown runner target value: %#v", jobpb.TargetRunner.Target)
	}

	// Timestamps
	timestamps := []struct {
		Field *time.Time
		Src   *timestamp.Timestamp
	}{
		{&rec.QueueTime, jobpb.QueueTime},
	}
	for _, ts := range timestamps {
		t, err := ptypes.Timestamp(ts.Src)
		if err != nil {
			return nil, err
		}

		*ts.Field = t
	}

	// If this job is assigned. Then we have to start a nacking timer.
	// We reset the nack timer so it gives runners time to reconnect.
	if rec.State == pb.Job_WAITING {
		// Create our timer to requeue this if it isn't acked
		rec.StateTimer = time.AfterFunc(jobWaitingTimeout, func() {
			s.JobAck(rec.Id, false)
		})
	}

	// If this job is running, we need to restart a heartbeat timeout.
	// This should only happen on reinit. This is tested.
	if rec.State == pb.Job_RUNNING {
		rec.StateTimer = time.AfterFunc(jobHeartbeatTimeout, func() {
			// Force cancel
			s.JobCancel(rec.Id, true)
		})
	}

	// If we have an expiry, we need to set a timer to expire this job.
	if jobpb.ExpireTime != nil {
		now := time.Now()

		t, err := ptypes.Timestamp(jobpb.ExpireTime)
		if err != nil {
			return nil, err
		}

		dur := t.Sub(now)
		if dur < 0 {
			dur = 1
		}

		time.AfterFunc(dur, func() { s.JobExpire(jobpb.Id) })
	}

	// Insert the index
	return rec, txn.Insert(jobTableName, rec)
}

func (s *State) jobCreate(dbTxn *bolt.Tx, memTxn *memdb.Txn, jobpb *pb.Job) error {
	// Setup our initial job state
	var err error
	jobpb.State = pb.Job_QUEUED
	jobpb.QueueTime, err = ptypes.TimestampProto(time.Now())
	if err != nil {
		return err
	}

	id := []byte(jobpb.Id)
	bucket := dbTxn.Bucket(jobBucket)

	// If singleton ID is set, we need to delete (cancel) any previous job
	// with the same singleton ID if it is still queued.
	if jobpb.SingletonId != "" {
		result, err := memTxn.First(
			jobTableName,
			jobSingletonIdIndexName,
			jobpb.SingletonId,
			pb.Job_QUEUED,
		)
		if err != nil {
			return err
		}

		if result != nil {
			// Note we don't have to worry about jobAssignedSet here because
			// we only run this block of code if the job is in the QUEUED state.

			// Note we don't need to Copy here like other places because
			// we never modify this old jobIndex in-place.
			old := result.(*jobIndex)

			oldpb, err := s.jobById(dbTxn, old.Id)
			if err != nil {
				return err
			}
			oldpb.State = pb.Job_ERROR
			oldpb.Error = status.Newf(codes.Canceled,
				"replaced by job %s", id).Proto()

			// Update the index and data
			if err := dbPut(bucket, []byte(old.Id), oldpb); err != nil {
				return err
			}
			if _, err = s.jobIndexSet(memTxn, []byte(old.Id), oldpb); err != nil {
				return err
			}

			// Copy the queue time from the old one so we retain our position
			jobpb.QueueTime = oldpb.QueueTime
		}
	}

	// Insert into bolt
	if err := dbPut(dbTxn.Bucket(jobBucket), id, jobpb); err != nil {
		return err
	}

	// Insert into the DB
	_, err = s.jobIndexSet(memTxn, id, jobpb)
	if err != nil {
		return err
	}

	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()

	s.indexedJobs++

	return nil
}

func (s *State) jobsPruneOld(memTxn *memdb.Txn, max int) (int, error) {
	return pruneOld(memTxn, pruneOp{
		lock:      &s.pruneMu,
		table:     jobTableName,
		index:     jobQueueTimeIndexName,
		indexArgs: []interface{}{pb.Job_QUEUED, time.Unix(0, 0)},
		max:       max,
		cur:       &s.indexedJobs,
		check: func(raw interface{}) bool {
			job := raw.(*jobIndex)
			return !jobIsCompleted(job.State)
		},
	})
}

func (s *State) jobById(dbTxn *bolt.Tx, id string) (*pb.Job, error) {
	var result pb.Job
	b := dbTxn.Bucket(jobBucket)
	return &result, dbGet(b, []byte(id), &result)
}

func (s *State) jobReadAndUpdate(id string, f func(*pb.Job) error) (*pb.Job, error) {
	var result *pb.Job
	var err error
	return result, s.db.Update(func(dbTxn *bolt.Tx) error {
		result, err = s.jobById(dbTxn, id)
		if err != nil {
			return err
		}

		// Modify
		if err := f(result); err != nil {
			return err
		}

		// Commit
		return dbPut(dbTxn.Bucket(jobBucket), []byte(id), result)
	})
}

// jobCandidateById returns the most promising candidate job to assign
// that is targeting a specific runner by ID.
func (s *State) jobCandidateById(memTxn *memdb.Txn, ws memdb.WatchSet, r *runnerRecord) (*jobIndex, error) {
	iter, err := memTxn.LowerBound(
		jobTableName,
		jobTargetIdIndexName,
		pb.Job_QUEUED,
		r.Id,
		time.Unix(0, 0),
	)
	if err != nil {
		return nil, err
	}

	for {
		raw := iter.Next()
		if raw == nil {
			break
		}

		job := raw.(*jobIndex)
		if job.State != pb.Job_QUEUED || job.TargetRunnerId != r.Id {
			continue
		}

		// If this job is blocked, it is not a candidate.
		if blocked, err := s.jobIsBlocked(memTxn, job, ws); err != nil {
			return nil, err
		} else if blocked {
			continue
		}

		return job, nil
	}

	return nil, nil
}

// jobCandidateAny returns the first candidate job that targets any runner.
func (s *State) jobCandidateAny(memTxn *memdb.Txn, ws memdb.WatchSet, r *runnerRecord) (*jobIndex, error) {
	iter, err := memTxn.LowerBound(
		jobTableName,
		jobQueueTimeIndexName,
		pb.Job_QUEUED,
		time.Unix(0, 0),
	)
	if err != nil {
		return nil, err
	}

	for {
		raw := iter.Next()
		if raw == nil {
			break
		}

		job := raw.(*jobIndex)
		if job.State != pb.Job_QUEUED || !job.TargetAny {
			continue
		}

		// If this job is blocked, it is not a candidate.
		if blocked, err := s.jobIsBlocked(memTxn, job, ws); err != nil {
			return nil, err
		} else if blocked {
			continue
		}

		return job, nil
	}

	return nil, nil
}

// Copy should be called prior to any modifications to an existing jobIndex.
func (idx *jobIndex) Copy() *jobIndex {
	// A shallow copy is good enough since we only modify top-level fields.
	copy := *idx
	return &copy
}

// Job returns the Job for an index.
func (idx *jobIndex) Job(jobpb *pb.Job) *Job {
	return &Job{
		Job:          jobpb,
		OutputBuffer: idx.OutputBuffer,
	}
}

// End notes this job is complete and performs any cleanup on the index.
func (idx *jobIndex) End() {
	if idx.StateTimer != nil {
		idx.StateTimer.Stop()
		idx.StateTimer = nil
	}
}
