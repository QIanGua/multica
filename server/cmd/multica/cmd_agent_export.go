package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// agentExportCmd writes agents' portable configuration to a JSON document that
// `agent import` reads back. Like `agent copy` it is a thin composition over
// existing endpoints — GET the agents, project them onto the portable field set
// — so it needs no dedicated server API.
var agentExportCmd = &cobra.Command{
	Use:   "export [agent-id...]",
	Short: "Export agent configuration to a portable JSON document",
	Long: `Export one or more agents' portable configuration as a JSON document.

The document is written to stdout by default, or to --file. Feed it back to
'multica agent import' to recreate the agents in this or another workspace.

Exported: name, description, instructions, avatar, model, thinking_level,
service_tier, custom_args, max_concurrent_tasks, invocation permission
(permission_mode + allow-list) and assigned workspace skills — the same field
set 'agent copy' treats as portable.

NEVER exported: custom_env, mcp_config and runtime_config. They are secret or
machine-local, and the API redacts or masks them on read anyway. The document
records only that they existed, so an import can tell you what still needs to
be supplied on the new agent.`,
	Args: cobra.ArbitraryArgs,
	RunE: runAgentExport,
}

func init() {
	agentCmd.AddCommand(agentExportCmd)
	registerAgentExportFlags(agentExportCmd)
}

// registerAgentExportFlags registers every flag runAgentExport reads. Shared
// between init() and the tests so both stay in lockstep.
func registerAgentExportFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("all", false, "Export every non-archived agent in the workspace instead of named ids")
	cmd.Flags().String("file", "", "Write the document to this path (mode 0600) instead of stdout")
	cmd.Flags().String("output", "json", "Output format for the --file summary: table or json")
}

func runAgentExport(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	switch {
	case all && len(args) > 0:
		return fmt.Errorf("pass either agent ids or --all, not both")
	case !all && len(args) == 0:
		return fmt.Errorf("specify at least one agent id, or --all to export every agent in the workspace")
	}

	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	ids := args
	if all {
		ids, err = listExportableAgentIDs(ctx, client)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("no agents to export in this workspace")
		}
	}

	doc := agentConfigDoc{
		Kind:    agentConfigDocKind,
		Version: agentConfigDocVersion,
		Source: agentConfigSource{
			WorkspaceID: client.WorkspaceID,
			ExportedAt:  time.Now().UTC().Format(time.RFC3339),
		},
		Agents: make([]agentConfigEntry, 0, len(ids)),
	}
	for _, id := range ids {
		var src map[string]any
		if err := client.GetJSON(ctx, "/api/agents/"+url.PathEscape(id), &src); err != nil {
			return fmt.Errorf("get agent %s: %w", id, err)
		}
		doc.Agents = append(doc.Agents, buildAgentConfigEntry(src))
	}

	filePath, _ := cmd.Flags().GetString("file")
	if filePath == "" {
		return cli.PrintJSON(cmd.OutOrStdout(), doc)
	}

	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode document: %w", err)
	}
	// 0600: the document carries no credentials by construction, but an agent's
	// instructions are the owner's own material and there is no reason for the
	// rest of the machine to read them by default.
	if err := os.WriteFile(filePath, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filePath, err)
	}

	names := make([]string, 0, len(doc.Agents))
	for _, a := range doc.Agents {
		names = append(names, a.Name)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(cmd.OutOrStdout(), map[string]any{
			"file":   filePath,
			"count":  len(doc.Agents),
			"agents": names,
		})
	}

	rows := make([][]string, 0, len(doc.Agents))
	for _, a := range doc.Agents {
		runtimeID := ""
		if a.Origin != nil {
			runtimeID = a.Origin.RuntimeID
		}
		rows = append(rows, []string{a.Name, a.Model, runtimeID})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Exported %d agent(s) to %s\n", len(doc.Agents), filePath)
	cli.PrintTable(cmd.OutOrStdout(), []string{"NAME", "MODEL", "SOURCE_RUNTIME"}, rows)
	return nil
}

// listExportableAgentIDs returns the ids of every non-archived agent in the
// workspace. Archived agents are excluded: --all is "export what this workspace
// runs today", and an archived agent would import as an active one.
func listExportableAgentIDs(ctx context.Context, client *cli.APIClient) ([]string, error) {
	if client.WorkspaceID == "" {
		return nil, fmt.Errorf("workspace_id is required for --all: use --workspace-id, set MULTICA_WORKSPACE_ID, or run 'multica config set workspace_id <id>'")
	}

	params := url.Values{}
	params.Set("workspace_id", client.WorkspaceID)

	var agents []map[string]any
	if err := client.GetJSON(ctx, "/api/agents?"+params.Encode(), &agents); err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		if strVal(a, "archived_at") != "" {
			continue
		}
		if id := strVal(a, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
