package cmd

import (
	"razpravljalnica/client"

	"github.com/spf13/cobra"
)

// var clientControlPlane string
var cpAddrs = []string{
	"localhost:6000",
	"localhost:6001",
	"localhost:6002",
	"localhost:6003",
	"localhost:6004",
}

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Start client UI",
	RunE: func(cmd *cobra.Command, args []string) error {
		client.Run(&cpAddrs)
		return nil
	},
}

func init() {
	//simple --flags premaknjeni sm
	//clientCmd.Flags().StringVar(&clientControlPlane, "control-plane", "6000", "Control plane address")
	rootCmd.AddCommand(clientCmd)
}
