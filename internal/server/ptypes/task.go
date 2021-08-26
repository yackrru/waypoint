package ptypes

import (
	"github.com/go-ozzo/ozzo-validation/v4"
	"github.com/imdario/mergo"
	"github.com/mitchellh/go-testing-interface"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/waypoint/internal/pkg/validationext"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
)

// TestTask returns a valid user for tests.
func TestTask(t testing.T, src *pb.Task) *pb.Task {
	t.Helper()

	if src == nil {
		src = &pb.Task{}
	}

	require.NoError(t, mergo.Merge(src, &pb.Task{
		Id: "test",
	}))

	return src
}

// ValidateTask validates the user structure.
func ValidateTask(v *pb.Task) error {
	return validationext.Error(validation.ValidateStruct(v,
		ValidateTaskRules(v)...,
	))
}

// ValidateTaskRules
func ValidateTaskRules(v *pb.Task) []*validation.FieldRules {
	return []*validation.FieldRules{
		validation.Field(&v.Id, validation.Required),
	}
}
