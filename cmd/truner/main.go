// Command truner installs and manages truner, ephemeral GitHub Actions
// runners in your own AWS account.
package main

import (
	"os"

	"github.com/codician-team/truner/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
