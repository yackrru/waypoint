package state

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/hashicorp/waypoint/internal/server/gen"
	serverptypes "github.com/hashicorp/waypoint/internal/server/ptypes"
)

func TestTask(t *testing.T) {
	t.Run("Get returns not found error if not exist", func(t *testing.T) {
		require := require.New(t)

		s := TestState(t)
		defer s.Close()

		_, err := s.TaskGet(&pb.Ref_Task{Id: "foo"})
		require.Error(err)
		require.Equal(codes.NotFound, status.Code(err))
	})

	t.Run("Put and Get", func(t *testing.T) {
		require := require.New(t)

		s := TestState(t)
		defer s.Close()

		// Set
		err := s.TaskPut(serverptypes.TestTask(t, &pb.Task{
			Id: "foo",
		}))
		require.NoError(err)

		// Get by name
		{
			resp, err := s.TaskGet(&pb.Ref_Task{Id: "foo"})
			require.NoError(err)
			require.NotNil(resp)
		}

		// Get by name, case insensitive
		{
			resp, err := s.TaskGet(&pb.Ref_Task{Id: "Foo"})
			require.NoError(err)
			require.NotNil(resp)
		}

		// List
		{
			resp, err := s.TaskList()
			require.NoError(err)
			require.NotNil(resp)
			require.Len(resp, 1)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		require := require.New(t)

		s := TestState(t)
		defer s.Close()

		// We need two methods
		require.NoError(s.TaskPut(serverptypes.TestTask(t, &pb.Task{
			Id: "bar",
		})))

		// Set
		err := s.TaskPut(serverptypes.TestTask(t, &pb.Task{
			Id: "baz",
		}))
		require.NoError(err)

		// Read
		resp, err := s.TaskGet(&pb.Ref_Task{Id: "bar"})
		require.NoError(err)
		require.NotNil(resp)

		// Delete
		{
			err := s.TaskDelete(&pb.Ref_Task{Id: "bar"})
			require.NoError(err)
		}

		// Read
		{
			_, err := s.TaskGet(&pb.Ref_Task{Id: "bar"})
			require.Error(err)
			require.Equal(codes.NotFound, status.Code(err))
		}

		// List
		{
			resp, err := s.TaskList()
			require.NoError(err)
			require.NotNil(resp)
			require.Len(resp, 1)
		}
	})
}
