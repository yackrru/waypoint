package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/waypoint/internal/core"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
	"github.com/hashicorp/waypoint/internal/server/singleprocess"
)

func TestRunnerInitOp(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	client := singleprocess.TestServer(t)
	log := testLog()

	// Create and start our runner
	runner, err := New(
		WithClient(client),
		WithLogger(log),
	)
	require.NoError(err)
	defer runner.Close()
	require.NoError(runner.Start())

	// Create a test project and store it in the db
	project := core.TestProject(t, core.WithClient(client))
	client.UpsertProject(ctx, &pb.UpsertProjectRequest{
		Project: &pb.Project{
			Name: project.Ref().Project,
		},
	})

	res, err := runner.executeInitOp(ctx, runner.logger, project)
	require.NoError(err)
	require.NotNil(t, res.Init)

	storedProject, err := getStoredProject(ctx, client, project.Ref())
	require.NoError(err)
	require.Len(
		storedProject.Applications,
		len(project.Apps()),
		"The project’s apps should have been upserted",
	)
}

func testLog() hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{
		Name:            "test-runner",
		Level:           hclog.Debug,
		IncludeLocation: true,
	})
}

func getStoredProject(
	ctx context.Context,
	client pb.WaypointClient,
	project *pb.Ref_Project,
) (*pb.Project, error) {
	res, err := client.GetProject(ctx, &pb.GetProjectRequest{
		Project: project,
	})

	if err != nil {
		return nil, err
	}

	return res.Project, nil
}
