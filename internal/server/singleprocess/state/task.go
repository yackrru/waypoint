package state

import (
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/go-memdb"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/hashicorp/waypoint/internal/server/gen"
)

var (
	taskBucket = []byte("tasks")
)

func init() {
	dbBuckets = append(dbBuckets, taskBucket)
	dbIndexers = append(dbIndexers, (*State).taskIndexInit)
	schemas = append(schemas, taskSchema)
}

// TaskPut creates or updates the given task. .
func (s *State) TaskPut(v *pb.Task) error {
	memTxn := s.inmem.Txn(true)
	defer memTxn.Abort()

	err := s.db.Update(func(dbTxn *bolt.Tx) error {
		return s.taskPut(dbTxn, memTxn, v)
	})
	if err == nil {
		memTxn.Commit()
	}

	return err
}

// TaskGet gets a task by reference.
func (s *State) TaskGet(ref *pb.Ref_Task) (*pb.Task, error) {
	memTxn := s.inmem.Txn(false)
	defer memTxn.Abort()

	var result *pb.Task
	err := s.db.View(func(dbTxn *bolt.Tx) error {
		var err error
		result, err = s.taskGet(dbTxn, memTxn, ref)
		return err
	})

	return result, err
}

// TaskDelete deletes an task by reference.
func (s *State) TaskDelete(ref *pb.Ref_Task) error {
	memTxn := s.inmem.Txn(true)
	defer memTxn.Abort()

	err := s.db.Update(func(dbTxn *bolt.Tx) error {
		return s.taskDelete(dbTxn, memTxn, ref)
	})
	if err == nil {
		memTxn.Commit()
	}

	return err
}

// TaskList returns the list of tasks.
func (s *State) TaskList() ([]*pb.Task, error) {
	memTxn := s.inmem.Txn(false)
	defer memTxn.Abort()

	iter, err := memTxn.Get(taskTableName, taskIdIndexName+"_prefix", "")
	if err != nil {
		return nil, err
	}

	var result []*pb.Task
	for {
		next := iter.Next()
		if next == nil {
			break
		}
		idx := next.(*taskIndex)

		var v *pb.Task
		err = s.db.View(func(dbTxn *bolt.Tx) error {
			v, err = s.taskGet(dbTxn, memTxn, &pb.Ref_Task{Id: idx.Id})
			return err
		})

		result = append(result, v)
	}

	return result, nil
}

func (s *State) taskPut(
	dbTxn *bolt.Tx,
	memTxn *memdb.Txn,
	value *pb.Task,
) error {
	id := s.taskId(value)

	// Get the global bucket and write the value to it.
	b := dbTxn.Bucket(taskBucket)
	if err := dbPut(b, id, value); err != nil {
		return err
	}

	// Create our index value and write that.
	return s.taskIndexSet(memTxn, id, value)
}

func (s *State) taskGet(
	dbTxn *bolt.Tx,
	memTxn *memdb.Txn,
	ref *pb.Ref_Task,
) (*pb.Task, error) {
	var result pb.Task
	b := dbTxn.Bucket(taskBucket)
	return &result, dbGet(b, []byte(strings.ToLower(ref.Id)), &result)
}

func (s *State) taskDelete(
	dbTxn *bolt.Tx,
	memTxn *memdb.Txn,
	ref *pb.Ref_Task,
) error {
	// Get the task. If it doesn't exist then we're successful.
	v, err := s.taskGet(dbTxn, memTxn, ref)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}

		return err
	}

	// Delete from bolt
	id := s.taskId(v)
	bucket := dbTxn.Bucket(taskBucket)
	if err := bucket.Delete(id); err != nil {
		return err
	}

	// Delete from memdb
	if err := memTxn.Delete(taskTableName, &taskIndex{Id: string(id)}); err != nil {
		return err
	}

	return nil
}

// taskIndexSet writes an index record for a single task.
func (s *State) taskIndexSet(txn *memdb.Txn, id []byte, value *pb.Task) error {
	record := &taskIndex{
		Id:    string(id),
		State: value.PhysicalState,
	}

	// Insert the index
	return txn.Insert(taskTableName, record)
}

// taskIndexInit initializes the task index from persisted data.
func (s *State) taskIndexInit(dbTxn *bolt.Tx, memTxn *memdb.Txn) error {
	bucket := dbTxn.Bucket(taskBucket)
	return bucket.ForEach(func(k, v []byte) error {
		var value pb.Task
		if err := proto.Unmarshal(v, &value); err != nil {
			return err
		}
		if err := s.taskIndexSet(memTxn, k, &value); err != nil {
			return err
		}

		return nil
	})
}

func (s *State) taskId(v *pb.Task) []byte {
	return []byte(strings.ToLower(v.Id))
}

func taskSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: taskTableName,
		Indexes: map[string]*memdb.IndexSchema{
			taskIdIndexName: {
				Name:         taskIdIndexName,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field:     "Id",
					Lowercase: true,
				},
			},
		},
	}
}

const (
	taskTableName   = "tasks"
	taskIdIndexName = "id"
)

type taskIndex struct {
	Id    string
	State pb.Operation_PhysicalState
}

// Copy should be called prior to any modifications to an existing record.
func (idx *taskIndex) Copy() *taskIndex {
	// A shallow copy is good enough since we only modify top-level fields.
	copy := *idx
	return &copy
}
