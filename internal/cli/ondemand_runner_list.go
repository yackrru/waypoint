package cli

import (
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/posener/complete"

	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	"github.com/hashicorp/waypoint/internal/clierrors"
	"github.com/hashicorp/waypoint/internal/pkg/flag"
)

type OnDemandRunnerConfigListCommand struct {
	*baseCommand
}

func (c *OnDemandRunnerConfigListCommand) Run(args []string) int {
	// Initialize. If we fail, we just exit since Init handles the UI.
	if err := c.Init(
		WithArgs(args),
		WithFlags(c.Flags()),
		WithNoConfig(),
	); err != nil {
		return 1
	}

	resp, err := c.project.Client().ListOnDemandRunnerConfigs(c.Ctx, &empty.Empty{})
	if err != nil {
		c.ui.Output(clierrors.Humanize(err), terminal.WithErrorStyle())
		return 1
	}

	if len(resp.Configs) == 0 {
		return 0
	}

	c.ui.Output("On-Demand Runner Configurations")

	tbl := terminal.NewTable("Id", "Plugin Type", "OCI Url", "Default")

	for _, p := range resp.Configs {
		def := ""
		if p.Default {
			def = "yes"
		}

		tbl.Rich([]string{
			p.Id,
			p.PluginType,
			p.OciUrl,
			def,
		}, nil)
	}

	c.ui.Table(tbl)

	return 0
}

func (c *OnDemandRunnerConfigListCommand) Flags() *flag.Sets {
	return c.flagSet(0, nil)
}

func (c *OnDemandRunnerConfigListCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

func (c *OnDemandRunnerConfigListCommand) AutocompleteFlags() complete.Flags {
	return c.Flags().Completions()
}

func (c *OnDemandRunnerConfigListCommand) Synopsis() string {
	return "List all registered on-demand runner configurations."
}

func (c *OnDemandRunnerConfigListCommand) Help() string {
	return formatHelp(`
Usage: waypoint runner list

  List all information on runners.

  On-Demand runner configuration is used to dynamically start tasks to execute operations for
  projects such as building, deploying, etc.
`)
}
