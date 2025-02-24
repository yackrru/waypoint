package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/command/image/build"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/waypoint-plugin-sdk/component"
	"github.com/hashicorp/waypoint-plugin-sdk/docs"
	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	"github.com/moby/buildkit/session"
	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	wpdockerclient "github.com/hashicorp/waypoint/builtin/docker/client"
	"github.com/hashicorp/waypoint/internal/assets"
	"github.com/hashicorp/waypoint/internal/pkg/epinject"
)

const minBuildkitDockerVersion = "1.39"

// Builder uses `docker build` to build a Docker image.
type Builder struct {
	config BuilderConfig
}

// BuildFunc implements component.Builder
func (b *Builder) BuildFunc() interface{} {
	return b.Build
}

// BuildFunc implements component.BuilderODR
func (b *Builder) BuildODRFunc() interface{} {
	return b.BuildODR
}

// BuilderConfig is the configuration structure for the builder
type BuilderConfig struct {
	// Control whether or not to inject the entrypoint binary into the resulting image
	DisableCEB bool `hcl:"disable_entrypoint,optional"`

	// Controls whether or not the image should be build with buildkit or docker v1
	UseBuildKit bool `hcl:"buildkit,optional"`

	// The name/path to the Dockerfile if it is not the root of the project
	Dockerfile string `hcl:"dockerfile,optional"`

	// Controls the passing of platform flag variables
	Platform string `hcl:"platform,optional"`

	// Controls the passing of build time variables
	BuildArgs map[string]*string `hcl:"build_args,optional"`

	// Controls the passing of build context
	Context string `hcl:"context,optional"`
}

func (b *Builder) Documentation() (*docs.Documentation, error) {
	doc, err := docs.New(
		docs.FromConfig(&BuilderConfig{}),
		docs.FromFunc(b.BuildFunc()),
	)
	if err != nil {
		return nil, err
	}

	doc.Description(`
Build a Docker image from a Dockerfile.

If a Docker server is available (either locally or via environment variables
such as "DOCKER_HOST"), then "docker build" will be used to build an image
from a Dockerfile.

### Dockerless Builds

Many hosted environments, such as Kubernetes clusters, don't provide access
to a Docker server. In these cases, it is desirable to perform what is called
a "dockerless" build: building a Docker image without access to a Docker
daemon. Waypoint supports dockerless builds.

Waypoint will automatically attempt a dockerless build if a Docker daemon
is not available and no remote Docker server environment variables are set.

Dockerless builds require user namespaces to be enabled. This is a host-level
setting that is often not enabled by default. For GKE, you must not use ContainerOS.
For AKS (Azure) and EKS (AWS), you must use a custom AMI that has user namespaces
enabled. Please search for how to enable user namespaces for your distro; it is
usually a single line configuration.
`)

	doc.Example(`
build {
  use "docker" {
	buildkit    = false
	disable_entrypoint = false
  }
}
`)

	doc.Output("docker.Image")

	doc.SetField(
		"disable_entrypoint",
		"if set, the entrypoint binary won't be injected into the image",
		docs.Summary(
			"The entrypoint binary is what provides extended functionality",
			"such as logs and exec. If it is not injected at build time",
			"the expectation is that the image already contains it",
		),
	)

	doc.SetField(
		"buildkit",
		"if set, use the buildkit builder from Docker",
	)

	doc.SetField(
		"dockerfile",
		"The path to the Dockerfile.",
		docs.Summary(
			"Set this when the Dockerfile is not APP-PATH/Dockerfile",
		),
	)

	doc.SetField(
		"build_args",
		"build args to pass to docker or img for the build step",
		docs.Summary(
			"An array of strings of build-time variables passed as build-arg to docker",
			" or img for the build step.",
		),
	)

	doc.SetField(
		"platform",
		"set target platform to build container if server is multi-platform capable",
		docs.Summary(
			"Must enable Docker buildkit to use the 'platform' flag.",
		),
	)

	doc.SetField(
		"context",
		"Build context path",
	)

	return doc, nil
}

// Config implements Configurable
func (b *Builder) Config() (interface{}, error) {
	return &b.config, nil
}

// Build
func (b *Builder) BuildODR(
	ctx context.Context,
	ui terminal.UI,
	src *component.Source,
	log hclog.Logger,
	ai *AccessInfo,
) (*Image, error) {
	sg := ui.StepGroup()
	defer sg.Wait()

	dockerfile := b.config.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(src.Path, dockerfile)
	}

	// If the dockerfile is outside of our build context, then we copy it
	// into our build context.
	relDockerfile, err := filepath.Rel(src.Path, dockerfile)
	if err != nil || strings.HasPrefix(relDockerfile, "..") {
		id, err := ulid.New(ulid.Now(), rand.Reader)
		if err != nil {
			return nil, err
		}

		newPath := filepath.Join(src.Path, fmt.Sprintf("Dockerfile-%s", id.String()))
		if err := copyFile(dockerfile, newPath); err != nil {
			return nil, err
		}
		defer os.Remove(newPath)

		dockerfile = newPath
	}

	path := src.Path

	if b.config.Context != "" {
		path = b.config.Context
	}

	contextDir, relDockerfile, err := build.GetContextFromLocalDir(path, dockerfile)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unable to create Docker context: %s", err)
	}
	log.Debug("loaded Docker context",
		"context_dir", contextDir,
		"dockerfile", relDockerfile,
	)

	log.Info("executing build via kaniko")

	result, err := b.buildWithKaniko(ctx, ui, sg, log, relDockerfile, contextDir, b.config.BuildArgs, ai)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// Build
func (b *Builder) Build(
	ctx context.Context,
	ui terminal.UI,
	src *component.Source,
	log hclog.Logger,
) (*Image, error) {
	sg := ui.StepGroup()
	defer sg.Wait()
	step := sg.Add("Initializing Docker client...")
	defer func() {
		if step != nil {
			step.Abort()
		}
	}()

	result := &Image{
		Image:    fmt.Sprintf("waypoint.local/%s", src.App),
		Tag:      "latest",
		Location: &Image_Docker{Docker: &empty.Empty{}},
	}

	cli, err := wpdockerclient.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unable to create Docker client: %s", err)
	}
	cli.NegotiateAPIVersion(ctx)

	dockerfile := b.config.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfile) {
		dockerfile = filepath.Join(src.Path, dockerfile)
	}

	// If the dockerfile is outside of our build context, then we copy it
	// into our build context.
	relDockerfile, err := filepath.Rel(src.Path, dockerfile)
	if err != nil || strings.HasPrefix(relDockerfile, "..") {
		id, err := ulid.New(ulid.Now(), rand.Reader)
		if err != nil {
			return nil, err
		}

		newPath := filepath.Join(src.Path, fmt.Sprintf("Dockerfile-%s", id.String()))
		if err := copyFile(dockerfile, newPath); err != nil {
			return nil, err
		}
		defer os.Remove(newPath)

		dockerfile = newPath
	}

	path := src.Path

	if b.config.Context != "" {
		path = b.config.Context
	}

	contextDir, relDockerfile, err := build.GetContextFromLocalDir(path, dockerfile)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unable to create Docker context: %s", err)
	}
	log.Debug("loaded Docker context",
		"context_dir", contextDir,
		"dockerfile", relDockerfile,
	)

	// We now test if Docker is actually functional. We do this here because we
	// need all of the above to complete the actual build.
	log.Debug("testing if we should use a Docker fallback")
	useImg := false
	if fallback, err := wpdockerclient.Fallback(ctx, log, cli); err != nil {
		log.Warn("error during check if we should use Docker fallback", "err", err)
		return nil, status.Errorf(codes.Internal,
			"error validating Docker connection: %s", err)
	} else if fallback && HasImg() {
		// If we're falling back and have "img" available, use that. If we
		// don't have "img" available, we continue to try to use Docker. We'll
		// fail but that error message should help the user.
		step.Update("Docker isn't available. Falling back to daemonless image build...")
		step.Done()
		step = nil
		if err := b.buildWithImg(
			ctx, ui, sg, relDockerfile, contextDir, result.Name(),
			b.config.BuildArgs,
		); err != nil {
			return nil, err
		}

		// Our image is in the img registry now. We set this so that
		// future users of this result type know where to look.
		result.Location = &Image_Img{Img: &empty.Empty{}}

		// We set this to true so we use the img-based injector later
		useImg = true
	} else {
		// No fallback, build with Docker
		step.Done()
		step = nil
		if err := b.buildWithDocker(
			ctx, ui, sg, cli, contextDir, relDockerfile, result.Name(), b.config.Platform, b.config.BuildArgs, log,
		); err != nil {
			return nil, err
		}
	}

	if !b.config.DisableCEB {
		step = sg.Add("Injecting Waypoint Entrypoint...")

		asset, err := assets.Asset("ceb/ceb")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to restore custom entry point binary: %s", err)
		}

		assetInfo, err := assets.AssetInfo("ceb/ceb")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to restore custom entry point binary: %s", err)
		}

		callback := func(cur []string) (*epinject.NewEntrypoint, error) {
			ep := &epinject.NewEntrypoint{
				Entrypoint: append([]string{"/waypoint-entrypoint"}, cur...),
				InjectFiles: map[string]epinject.InjectFile{
					"/waypoint-entrypoint": {
						Reader: bytes.NewReader(asset),
						Info:   assetInfo,
					},
				},
			}

			return ep, nil
		}

		if !useImg {
			_, err = epinject.AlterEntrypoint(ctx, result.Name(), callback)
		} else {
			_, err = epinject.AlterEntrypointImg(ctx, result.Name(), callback)
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to set modify Docker entrypoint: %s", err)
		}

		step.Done()
	}

	return result, nil
}

func (b *Builder) buildWithDocker(
	ctx context.Context,
	ui terminal.UI,
	sg terminal.StepGroup,
	cli *client.Client,
	contextDir string,
	relDockerfile string,
	tag string,
	platform string,
	buildArgs map[string]*string,
	log hclog.Logger,
) error {
	excludes, err := build.ReadDockerignore(contextDir)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to read .dockerignore: %s", err)
	}

	if err := build.ValidateContextDirectory(contextDir, excludes); err != nil {
		return status.Errorf(codes.Internal, "error checking context: %s", err)
	}

	// And canonicalize dockerfile name to a platform-independent one
	relDockerfile = archive.CanonicalTarNameForPath(relDockerfile)

	excludes = build.TrimBuildFilesFromExcludes(excludes, relDockerfile, false)
	buildCtx, err := archive.TarWithOptions(contextDir, &archive.TarOptions{
		ExcludePatterns: excludes,
		ChownOpts:       &idtools.Identity{UID: 0, GID: 0},
	})

	if err != nil {
		return status.Errorf(codes.Internal, "unable to compress context: %s", err)
	}

	ver := types.BuilderV1
	if b.config.UseBuildKit {
		ver = types.BuilderBuildKit
	}

	step := sg.Add("Building image...")
	defer step.Abort()

	stdout, _, err := ui.OutputWriters()
	if err != nil {
		return err
	}

	if platform != "" && ver != types.BuilderBuildKit {
		return status.Errorf(codes.InvalidArgument, "buildkit is required to use platform option")
	}

	buildOpts := types.ImageBuildOptions{
		Version:    ver,
		Dockerfile: relDockerfile,
		Tags:       []string{tag},
		Remove:     true,
		Platform:   platform,
		BuildArgs:  buildArgs,
	}

	// Buildkit builds need a session under most circumstances, but sessions are only supported in >1.39
	if ver == types.BuilderBuildKit {
		dockerClientVersion := cli.ClientVersion()
		if !versions.GreaterThanOrEqualTo(dockerClientVersion, minBuildkitDockerVersion) {
			log.Warn("Buildkit requested and docker engine does not support sessions, so not using a session",
				"dockerClientVersion", dockerClientVersion,
				"minBuildkitDockerVersion", minBuildkitDockerVersion,
			)
		} else {
			s, _ := session.NewSession(ctx, "waypoint", "")

			dialSession := func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
				return cli.DialHijack(ctx, "/session", proto, meta)
			}

			go s.Run(ctx, dialSession)
			defer s.Close()

			buildOpts.SessionID = s.ID()
		}
	}

	resp, err := cli.ImageBuild(ctx, buildCtx, buildOpts)
	if err != nil {
		return status.Errorf(codes.Internal, "error building image: %s", err)
	}
	defer resp.Body.Close()

	var termFd uintptr
	if f, ok := stdout.(*os.File); ok {
		termFd = f.Fd()
	}

	err = jsonmessage.DisplayJSONMessagesStream(resp.Body, step.TermOutput(), termFd, true, nil)
	if err != nil {
		return status.Errorf(codes.Internal, "unable to stream build logs to the terminal: %s", err)
	}

	step.Done()
	return nil
}
