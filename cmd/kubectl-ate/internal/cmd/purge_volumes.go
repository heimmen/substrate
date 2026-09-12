// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/spf13/cobra"
)

var purgeTemplateFlag string
var purgeAtespaceFlag string

var purgeCmd = &cobra.Command{
	Use:   "purge",
	Short: "Permanently remove persisted resources",
}

var purgeVolumesCmd = &cobra.Command{
	Use:   "volumes <actor-name>",
	Short: "Permanently remove an actor's persistent volume data",
	Long: `Permanently remove the backing storage of an actor's external volumes.

Unlike "kubectl-ate delete actor" (which keeps sticky volume data so a
delete+recreate refresh re-attaches it), this reclaims the storage for
good. The actor must not be RUNNING. Volume IDs are derived from the
ActorTemplate's external volume templates, so the actor record itself
may already be deleted.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		c, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, traceEnabled)
		if err != nil {
			return err
		}
		defer c.Close()

		name := args[0]
		parts := strings.Split(purgeTemplateFlag, "/")
		if len(parts) != 2 {
			return fmt.Errorf("malformed --template: %s (expected <namespace>/<name>)", purgeTemplateFlag)
		}

		resp, err := c.ControlClient.PurgeActorVolumes(ctx, &ateapipb.PurgeActorVolumesRequest{
			Actor:    &ateapipb.ObjectRef{Atespace: purgeAtespaceFlag, Name: name},
			Template: &ateapipb.ObjectRef{Atespace: parts[0], Name: parts[1]},
		})
		if err != nil {
			return err
		}

		for _, id := range resp.GetPurgedVolumeIds() {
			fmt.Printf("volume %q purged\n", id)
		}
		if len(resp.GetPurgedVolumeIds()) == 0 {
			fmt.Printf("no persistent volumes found for actor %q\n", name)
		}
		return nil
	},
}

func init() {
	purgeVolumesCmd.Flags().StringVarP(&purgeAtespaceFlag, "atespace", "a", "", "Atespace the actor lives in")
	_ = purgeVolumesCmd.MarkFlagRequired("atespace")
	purgeVolumesCmd.Flags().StringVarP(&purgeTemplateFlag, "template", "t", "", "ActorTemplate the actor was created from (<namespace>/<name>)")
	_ = purgeVolumesCmd.MarkFlagRequired("template")
	purgeCmd.AddCommand(purgeVolumesCmd)
	rootCmd.AddCommand(purgeCmd)
}
