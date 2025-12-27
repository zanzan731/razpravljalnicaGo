package cmd

import (
	"razpravljalnica/controlplane"

	"github.com/spf13/cobra"
)

var cpAddr string
var raftAddr string // tako kot je zdaj implementirano mora bit nujno cpAddr = raftAddr-1000
var nodeID string
var bootstrap bool        // s tem definiraš kateri bo prvi leader
var leaderGrpcAddr string // vsem podaš naslov prvegaleaderja

var controlPlaneCmd = &cobra.Command{
	Use:   "control-plane",
	Short: "Start control plane",
	RunE: func(cmd *cobra.Command, args []string) error {
		controlplane.Run(cpAddr, raftAddr, nodeID, bootstrap, leaderGrpcAddr)
		return nil
	},
}

func init() {
	//simple --flags premaknjeni sm
	//dodamo controlPlane command da ma tud on en command
	controlPlaneCmd.Flags().StringVar(&cpAddr, "addr", "localhost:6000", "Listen address")
	controlPlaneCmd.Flags().StringVar(&raftAddr, "raft", "localhost:7000", "Raft address")
	controlPlaneCmd.Flags().StringVar(&nodeID, "id", "1", "Node ID")
	controlPlaneCmd.Flags().BoolVar(&bootstrap, "bs", false, "True only for leader")
	controlPlaneCmd.Flags().StringVar(&leaderGrpcAddr, "leader", "localhost:6000", "Leader grpc address")
	rootCmd.AddCommand(controlPlaneCmd)
}
