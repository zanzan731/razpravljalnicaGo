package cmd

import (
	"razpravljalnica/controlplane"

	"github.com/spf13/cobra"
)

var cpAddr string

var controlPlaneCmd = &cobra.Command{
	Use:   "control-plane",
	Short: "Start control plane",
	RunE: func(cmd *cobra.Command, args []string) error {
		controlplane.Run(cpAddr)
		return nil
	},
}

func init() {
	//simple --flags premaknjeni sm
	//dodamo controlPlane command da ma tud on en command
	controlPlaneCmd.Flags().StringVar(&cpAddr, "addr", "6000", "Listen address")
	rootCmd.AddCommand(controlPlaneCmd)
}
