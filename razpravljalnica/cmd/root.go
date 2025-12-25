package cmd //cmd.Execute() isce pole paket cmd in Execute funkcijo (to je znotraj main)

//ta specificen file je kot root command v cobri torej ta je tisti top-level pole pa so control-plane client in server ki so subcommandi
//zato lahko tudi vse spakiramo v en binary saj je flow od tle do sub commandov
import (
	"os"

	"github.com/spf13/cobra"
)

// vsi cobra commandi majo ta tip
var rootCmd = &cobra.Command{
	Use:   "razpravljalnica",
	Short: "Distributed message board system",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
