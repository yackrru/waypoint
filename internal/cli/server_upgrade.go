package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/posener/complete"

	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	clientpkg "github.com/hashicorp/waypoint/internal/client"
	"github.com/hashicorp/waypoint/internal/clierrors"
	"github.com/hashicorp/waypoint/internal/clisnapshot"
	"github.com/hashicorp/waypoint/internal/pkg/flag"
	pb "github.com/hashicorp/waypoint/internal/server/gen"
	"github.com/hashicorp/waypoint/internal/serverclient"
	"github.com/hashicorp/waypoint/internal/serverinstall"
)

type ServerUpgradeCommand struct {
	*baseCommand

	platform     string
	contextName  string
	snapshotName string
	skipSnapshot bool
	confirm      bool
}

func (c *ServerUpgradeCommand) initWriter(fileName string) (io.Writer, io.Closer, error) {
	f, err := os.Create(fileName)

	if err != nil {
		return nil, nil, err
	} else {
		return f, f, nil
	}
}

func (c *ServerUpgradeCommand) Run(args []string) int {
	ctx := c.Ctx
	log := c.Log.Named("upgrade")
	defer c.Close()

	// Initialize. If we fail, we just exit since Init handles the UI.
	if err := c.Init(
		WithArgs(args),
		WithFlags(c.Flags()),
		WithNoConfig(),
	); err != nil {
		return 1
	}

	// Error handling from input

	if !c.confirm {
		c.ui.Output(confirmReqMsg, terminal.WithErrorStyle())
		return 1
	}

	if c.platform == "" {
		c.ui.Output(
			"A platform is required and must match the server context",
			terminal.WithErrorStyle(),
		)
		return 1
	}

	p, ok := serverinstall.Platforms[strings.ToLower(c.platform)]
	if !ok {
		c.ui.Output(
			"Error upgrading server on %s: invalid platform",
			c.platform,
			terminal.WithErrorStyle(),
		)

		return 1
	}

	// Finish error handling

	// Get Server config to preserve existing configurations from context
	var ctxName string
	if c.contextName != "" {
		ctxName = c.contextName
	} else {
		defaultName, err := c.contextStorage.Default()
		if err != nil {
			c.ui.Output(
				"Error getting default context: %s",
				clierrors.Humanize(err),
				terminal.WithErrorStyle(),
			)
			return 1
		}
		ctxName = defaultName
	}

	originalCfg, err := c.contextStorage.Load(ctxName)
	if err != nil {
		c.ui.Output(
			"Error loading the context %q: %s",
			ctxName,
			clierrors.Humanize(err),
			terminal.WithErrorStyle(),
		)
		return 1
	}

	// Upgrade waypoint server
	sg := c.ui.StepGroup()
	defer sg.Wait()

	s := sg.Add("Validating server context: %q", ctxName)
	defer func() { s.Abort() }()

	conn, err := serverclient.Connect(ctx, serverclient.FromContextConfig(originalCfg))
	if err != nil {
		c.ui.Output(
			"Error connecting with context %q: %s",
			ctxName,
			clierrors.Humanize(err),
			terminal.WithErrorStyle(),
		)
		return 1
	}

	s.Update("Verifying connection is valid for context %q...", ctxName)

	client := pb.NewWaypointClient(conn)
	if _, err := clientpkg.New(ctx,
		clientpkg.WithLogger(c.Log),
		clientpkg.WithClient(client),
	); err != nil {
		c.ui.Output(
			"Error connecting with context %q: %s",
			ctxName,
			clierrors.Humanize(err),
			terminal.WithErrorStyle(),
		)
		return 1
	}

	resp, err := client.GetVersionInfo(ctx, &empty.Empty{})
	if err != nil {
		c.ui.Output(
			"Error retrieving server version info: %s", clierrors.Humanize(err),
			terminal.WithErrorStyle())
		return 1
	}

	initServerVersion := resp.Info.Version

	s.Update("Context %q validated and connected successfully.", ctxName)
	s.Done()

	s = sg.Add("Starting server snapshots")

	// Snapshot server before upgrade
	if !c.skipSnapshot {
		s.Update("Taking server snapshot before upgrading")

		snapshotName := fmt.Sprintf("%s-%d", defaultSnapshotName, time.Now().Unix())
		if c.snapshotName != "" {
			snapshotName = fmt.Sprintf("%s-%d", c.snapshotName, time.Now().Unix())
		}

		s.Update("Taking snapshot of server with name: '%s'", snapshotName)
		w, closer, err := c.initWriter(snapshotName)
		if err != nil {
			s.Update("Failed to take server snapshot")
			s.Status(terminal.StatusError)
			s.Done()

			c.ui.Output(fmt.Sprintf("Error opening output: %s", err), terminal.WithErrorStyle())
			return 1
		}

		if closer != nil {
			defer closer.Close()
		}

		if err = clisnapshot.WriteSnapshot(c.Ctx, c.project.Client(), w); err != nil {
			s.Update("Failed to take server snapshot")
			s.Status(terminal.StatusError)
			s.Done()

			c.ui.Output(fmt.Sprintf("Error generating Snapshot: %s", err), terminal.WithErrorStyle())
			return 1
		}

		s.Update("Snapshot of server written to: '%s'", snapshotName)
		s.Done()
	} else {
		s.Update("Server snapshot disabled on request, this means no snapshot will be taken before upgrades")
		s.Status(terminal.StatusWarn)
		s.Done()
		log.Warn("Server snapshot disabled on request from user, skipping")
	}

	c.ui.Output("Waypoint server will now upgrade from version %q",
		initServerVersion)

	c.ui.Output("Upgrading...", terminal.WithHeaderStyle())

	// Upgrade in place
	result, err := p.Upgrade(ctx, &serverinstall.InstallOpts{
		Log: log,
		UI:  c.ui,
	}, originalCfg.Server)
	if err != nil {
		// TODO(briancain): Add a bunch of help text on how to restore backup
		c.ui.Output(
			"Error upgrading server on %s: %s", c.platform, clierrors.Humanize(err),
			terminal.WithErrorStyle())

		c.ui.Output(upgradeFailHelp)

		return 1
	}

	contextConfig := result.Context
	advertiseAddr := result.AdvertiseAddr
	httpAddr := result.HTTPAddr

	originalCfg.Server.Address = contextConfig.Server.Address

	// Connect
	log.Info("connecting to the server so we can verify the server upgrade", "addr", originalCfg.Server.Address)
	conn, err = serverclient.Connect(ctx,
		serverclient.FromContextConfig(originalCfg),
		serverclient.Timeout(5*time.Minute),
	)
	if err != nil {
		c.ui.Output(
			"Error connecting to server: %s\n\n%s",
			clierrors.Humanize(err),
			errInstallRunning,
			terminal.WithErrorStyle(),
		)
		return 1
	}
	client = pb.NewWaypointClient(conn)

	resp, err = client.GetVersionInfo(ctx, &empty.Empty{})
	if err != nil {
		c.ui.Output(
			"Error retrieving server version info: %s", clierrors.Humanize(err),
			terminal.WithErrorStyle())
		return 1
	}

	if err := c.contextStorage.Set(ctxName, originalCfg); err != nil {
		c.ui.Output(
			"Error setting the CLI context: %s\n\n%s",
			clierrors.Humanize(err),
			errInstallRunning,
			terminal.WithErrorStyle(),
		)
		return 1
	}

	c.ui.Output("\nServer upgrade for platform %q context %q complete!",
		c.platform, ctxName, terminal.WithSuccessStyle())

	c.ui.Output("Waypoint has finished upgrading the server to version %q\n",
		resp.Info.Version, terminal.WithSuccessStyle())

	c.ui.Output(addrSuccess, advertiseAddr.Addr, "https://"+httpAddr,
		terminal.WithSuccessStyle())

	return 0
}

func (c *ServerUpgradeCommand) validateContext() bool {
	// TODO
	return true
}

func (c *ServerUpgradeCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Command Options")
		f.BoolVar(&flag.BoolVar{
			Name:    "auto-approve",
			Target:  &c.confirm,
			Default: false,
			Usage:   "Confirm server upgrade.",
		})
		f.StringVar(&flag.StringVar{
			Name:    "context-name",
			Target:  &c.contextName,
			Default: "",
			Usage:   "Waypoint server context to upgrade.",
		})
		f.StringVar(&flag.StringVar{
			Name:    "platform",
			Target:  &c.platform,
			Default: "",
			Usage:   "Platform to upgrade the Waypoint server from.",
		})
		f.StringVar(&flag.StringVar{
			Name:    "snapshot-name",
			Target:  &c.snapshotName,
			Default: "",
			Usage:   "Platform to upgrade the Waypoint server from.",
		})
		f.BoolVar(&flag.BoolVar{
			Name:    "skip-snapshot",
			Target:  &c.skipSnapshot,
			Default: false,
			Usage:   "Skip creating a snapshot of the Waypoint server.",
		})

		for name, platform := range serverinstall.Platforms {
			platformSet := set.NewSet(name + " Options")
			platform.UpgradeFlags(platformSet)
		}
	})
}

func (c *ServerUpgradeCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *ServerUpgradeCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *ServerUpgradeCommand) Synopsis() string {
	return "Upgrades Waypoint server in the current context to the latest version"
}

func (c *ServerUpgradeCommand) Help() string {
	return formatHelp(`
Usage: waypoint server upgrade [options]

	Upgrade Waypoint server in the current context to the latest version. This
	command will first take a snapshot of the running server, uninstall it, then
	install the new version using the restored snapshot. By default, Waypoint
	will upgrade to server version "hashicorp/waypoint:latest".

` + c.Flags().Help())
}

var (
	defaultSnapshotName = "waypoint-server-snapshot"
	confirmReqMsg       = strings.TrimSpace(`
Upgrading Waypoint server requires confirmation.
Rerun the command with '-auto-approve' to continue with the upgrade.
`)

	upgradeFailHelp = strings.TrimSpace(`
Upgrading Waypoint server has failed. To restore from a snapshot, use the command:

waypoint server restore [snapshot-name]

Where 'snapshot-name' is the name of the snapshot taken prior to the upgrade.

More information can be found by runninng 'waypoint server restore -help' or
following the server maintenence guide for backups and restores:
https://www.waypointproject.io/docs/server/run/maintenance#backup-restore
`)

	addrSuccess = strings.TrimSpace(`
Advertise Address: %[1]s
   Web UI Address: %[2]s
`)
)
