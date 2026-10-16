package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/getargvio/argvio/internal/tenant"
)

var memberRoleRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func newTenantMemberCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "member",
		Short: "Manage which OIDC identities may use a tenant",
		Long: `Manage tenant memberships: the (issuer, subject) pairs of OIDC users who may
use a tenant's dashboard. A user's access is resolved from the verified token's
iss and sub, never from a token claim.

--issuer must equal the provider's discovery "issuer" exactly, including any
trailing slash. Running servers cache memberships briefly, so a change can take
up to the tenant cache TTL to be seen.`,
	}
	cmd.AddCommand(newTenantMemberAddCommand(), newTenantMemberRemoveCommand(), newTenantMemberListCommand())
	return cmd
}

// memberTarget is the flag set shared by the member subcommands.
type memberTarget struct {
	dsn, tenantRef, issuer, subject string
}

func (m *memberTarget) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&m.dsn, "dsn", "", "Postgres DSN (defaults to $ARGVIO_STORAGE_DSN)")
	cmd.Flags().StringVar(&m.tenantRef, "tenant", "", "tenant id or slug (required)")
	cmd.Flags().StringVar(&m.issuer, "issuer", "", "OIDC issuer, exactly as the provider states it")
	cmd.Flags().StringVar(&m.subject, "sub", "", "OIDC subject (the token's sub claim)")
}

// open connects and resolves --tenant. The returned func closes the pool.
func (m *memberTarget) open(ctx context.Context) (*tenant.Store, *tenant.Tenant, func(), error) {
	if m.tenantRef == "" {
		return nil, nil, nil, errors.New("--tenant is required")
	}
	dsn, err := resolveDSN(m.dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	pool, err := connectPool(ctx, dsn)
	if err != nil {
		return nil, nil, nil, err
	}
	store := tenant.NewStore(pool)
	t, err := store.FindTenant(ctx, m.tenantRef)
	if errors.Is(err, tenant.ErrNotFound) {
		pool.Close()
		return nil, nil, nil, fmt.Errorf("no tenant with id or slug %q", m.tenantRef)
	}
	if err != nil {
		pool.Close()
		return nil, nil, nil, err
	}
	return store, t, pool.Close, nil
}

func newTenantMemberAddCommand() *cobra.Command {
	var target memberTarget
	var role string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add an identity to a tenant (idempotent)",
		Long: `Add an identity to a tenant. Repeating the command is a no-op; passing --role
for an existing member changes only the role. Without --role a new member is a "viewer".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if target.issuer == "" || target.subject == "" {
				return errors.New("--issuer and --sub are required")
			}
			if role != "" && !memberRoleRE.MatchString(role) {
				return fmt.Errorf("invalid --role %q: use lowercase letters, digits, - or _", role)
			}
			ctx := context.Background()
			store, t, closeFn, err := target.open(ctx)
			if err != nil {
				return err
			}
			defer closeFn()

			m, created, err := store.AddMember(ctx, t.ID, target.issuer, target.subject, role)
			if err != nil {
				return err
			}
			state := "is already a member of"
			if created {
				state = "added to"
			}
			fmt.Printf("%s %s %s as %s (%s)\n", m.Subject, state, t.Slug, m.Role, t.ID)
			return nil
		},
	}
	target.bind(cmd)
	cmd.Flags().StringVar(&role, "role", "", "member role (default \"viewer\" for a new member)")
	return cmd
}

func newTenantMemberRemoveCommand() *cobra.Command {
	var target memberTarget
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove an identity from a tenant",
		RunE: func(cmd *cobra.Command, args []string) error {
			if target.issuer == "" || target.subject == "" {
				return errors.New("--issuer and --sub are required")
			}
			ctx := context.Background()
			store, t, closeFn, err := target.open(ctx)
			if err != nil {
				return err
			}
			defer closeFn()

			removed, err := store.RemoveMember(ctx, t.ID, target.issuer, target.subject)
			if err != nil {
				return err
			}
			if !removed {
				fmt.Printf("%s was not a member of %s (%s)\n", target.subject, t.Slug, t.ID)
				return nil
			}
			fmt.Printf("removed %s from %s (%s)\n", target.subject, t.Slug, t.ID)
			return nil
		},
	}
	target.bind(cmd)
	return cmd
}

func newTenantMemberListCommand() *cobra.Command {
	var target memberTarget
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a tenant's members, optionally filtered by --issuer / --sub",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			store, t, closeFn, err := target.open(ctx)
			if err != nil {
				return err
			}
			defer closeFn()

			members, err := store.ListMembers(ctx, t.ID, target.issuer, target.subject)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				fmt.Printf("no members for %s (%s)\n", t.Slug, t.ID)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ISSUER\tSUBJECT\tROLE\tADDED")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.Issuer, m.Subject, m.Role, m.CreatedAt.UTC().Format("2006-01-02"))
			}
			return w.Flush()
		},
	}
	target.bind(cmd)
	return cmd
}
