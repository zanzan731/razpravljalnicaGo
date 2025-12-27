package cmd

import (
	"razpravljalnica/server"

	"github.com/spf13/cobra"
)

var serverAddr string

// var serverControlPlane string
// cpAddrs so definirani že v cmd/client.go

// za server
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start message board node",
	RunE: func(cmd *cobra.Command, args []string) error {
		server.Run(serverAddr, &cpAddrs)
		return nil
	},
}

func init() {
	//simple --flags premaknjeni sm
	serverCmd.Flags().StringVar(&serverAddr, "addr", "50051", "Listen address")
	//serverCmd.Flags().StringVar(&serverControlPlane, "control-plane", "6000", "Control plane address")
	rootCmd.AddCommand(serverCmd)
}
