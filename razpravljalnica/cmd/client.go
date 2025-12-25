package cmd

import (
	"razpravljalnica/client"

	"github.com/spf13/cobra"
)

var clientControlPlane string

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Start client UI",
	RunE: func(cmd *cobra.Command, args []string) error {
		client.Run(clientControlPlane)
		return nil
	},
}

func init() {
	//simple --flags premaknjeni sm
	clientCmd.Flags().StringVar(&clientControlPlane, "control-plane", "6000", "Control plane address")
	rootCmd.AddCommand(clientCmd)
}
