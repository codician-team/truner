package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
)

func (a *app) loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Save an AWS access key for truner (interactive)",
		Long: `Login asks for an AWS access key, checks it and saves it as the AWS profile
"truner", which later truner commands use automatically.

When an AI agent or a script runs truner for you, run this one command in
your own terminal, so the secret key never passes through the agent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.login(cmd.Context())
		},
	}
}

func (a *app) login(ctx context.Context) error {
	u := a.ui
	if !u.tty {
		return errors.New("truner login asks for a secret key, so it only runs in an interactive terminal")
	}
	o := a.aws.resolve()
	if cfg, err := loadAWS(ctx, o); err == nil {
		if id, err := whoami(ctx, cfg); err == nil {
			u.ok("AWS credentials already work: account %s as %s", id.Account, u.dim(id.Arn))
			if !u.confirm("Enter a different access key?", false) {
				return nil
			}
		}
	}
	_, id, o, err := promptAccessKey(ctx, u, o, true)
	if err != nil {
		return err
	}
	st := loadState()
	st.Profile, st.Region = o.Profile, o.Region
	if err := saveState(st); err != nil {
		return err
	}
	u.ok("Signed in to AWS account %s as %s", id.Account, u.dim(id.Arn))
	u.info("truner commands now use the AWS profile %q in %s. You can go back to your agent.", o.Profile, o.Region)
	return nil
}
