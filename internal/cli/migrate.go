package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/storage"
)

func newMigrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply or inspect database migrations",
	}
	cmd.AddCommand(
		newMigrateUpCommand(),
		newMigrateDownCommand(),
		newMigrateVersionCommand(),
	)
	return cmd
}

func newMigrateUpCommand() *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			if err := storage.MigrateUp(resolved); err != nil {
				return err
			}
			fmt.Println("migrations applied")
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	return cmd
}

func newMigrateDownCommand() *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Roll back all migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			if err := storage.MigrateDown(resolved); err != nil {
				return err
			}
			fmt.Println("migrations rolled back")
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	return cmd
}

func newMigrateVersionCommand() *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the current migration version",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveDSN(dsn)
			if err != nil {
				return err
			}
			v, dirty, err := storage.MigrationVersion(resolved)
			if err != nil {
				return err
			}
			fmt.Printf("version=%d dirty=%v\n", v, dirty)
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	return cmd
}
