package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sipeed/picoclaw/pkg/apiserve"
)

// keysFlags locates the SQLite file the running server uses.
type keysFlags struct {
	home string
	db   string
}

func (f *keysFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.home, "home", "", "picoclaw home (default: PICOCLAW_HOME)")
	cmd.Flags().StringVar(&f.db, "db", "", "path to apiserve.db (overrides --home)")
}

// openStore opens the shared SQLite file. It never starts the HTTP server and
// never builds an AgentLoop, so it is safe to run while the server is up —
// which is what makes this the emergency revocation path.
func (f *keysFlags) openStore() (*apiserve.Store, error) {
	home := f.home
	if home == "" {
		home = apiserve.ResolveHome()
	}
	dbPath := f.db
	if dbPath == "" {
		dbPath = apiserve.ResolveDBPath(home)
	}
	return apiserve.OpenStore(dbPath)
}

// defaultOwnerType keeps the stored owner_type non-empty.
func defaultOwnerType(v string) string {
	if strings.TrimSpace(v) == "" {
		return "internal_service"
	}
	return strings.TrimSpace(v)
}

// newKeysCommand builds the `keys` command group.
func newKeysCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys (create, list, enable, disable, rotate)",
		Long: "Manage the API keys stored in apiserve.db.\n" +
			"The plaintext of a key is printed exactly once, on create and rotate.",
	}
	cmd.AddCommand(
		newKeysCreateCommand(),
		newKeysListCommand(),
		newKeysEnableCommand(),
		newKeysDisableCommand(),
		newKeysRotateCommand(),
	)
	return cmd
}

func newKeysCreateCommand() *cobra.Command {
	flags := &keysFlags{}
	var (
		name          string
		ownerType     string
		tenantID      string
		allowedAgents []string
		allowedSkills []string
		rateLimit     string
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a key and print its plaintext once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := flags.openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			created, plaintext, err := store.CreateAPIKey(cmd.Context(), apiserve.CreateAPIKeyParams{
				Name:          name,
				OwnerType:     defaultOwnerType(ownerType),
				TenantID:      tenantID,
				AllowedAgents: allowedAgents,
				AllowedSkills: allowedSkills,
				RateLimit:     rateLimit,
			})
			if err != nil {
				return err
			}

			// The plaintext goes to stdout and nowhere else: not the logger, not
			// a file, not the database.
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:        %s\n", created.ID)
			fmt.Fprintf(out, "name:      %s\n", created.Name)
			fmt.Fprintf(out, "api_key:   %s\n", plaintext)
			fmt.Fprintln(out, "note: this plaintext is shown once and cannot be retrieved again")
			return nil
		},
	}

	flags.register(cmd)
	cmd.Flags().StringVar(&name, "name", "", `human readable name, e.g. "order-service"`)
	cmd.Flags().StringVar(&ownerType, "owner-type", "internal_service", "owner type")
	cmd.Flags().StringVar(&tenantID, "tenant", "", "tenant id (v2, optional)")
	cmd.Flags().StringSliceVar(&allowedAgents, "allowed-agents", nil,
		"comma separated agent ids (empty = unrestricted)")
	cmd.Flags().StringSliceVar(&allowedSkills, "allowed-skills", nil,
		"comma separated skill names (empty = unrestricted)")
	cmd.Flags().StringVar(&rateLimit, "rate-limit", "", `e.g. "20/s,8" (empty = unlimited)`)
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newKeysListCommand() *cobra.Command {
	flags := &keysFlags{}
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List keys (never prints plaintext or hashes)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := flags.openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			keys, err := store.ListAPIKeys(cmd.Context())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				// A dedicated row type, NOT apiserve.APIKey: serializing the
				// latter would leak key_hash. key_hash doubles as the callback
				// signing key, so leaking it is equivalent to allowing forged
				// callbacks.
				type row struct {
					ID            string   `json:"id"`
					Name          string   `json:"name"`
					OwnerType     string   `json:"owner_type"`
					Enabled       bool     `json:"enabled"`
					AllowedAgents []string `json:"allowed_agents"`
					AllowedSkills []string `json:"allowed_skills"`
					RateLimit     string   `json:"rate_limit"`
				}
				rows := make([]row, 0, len(keys))
				for _, key := range keys {
					rows = append(rows, row{
						ID:            key.ID,
						Name:          key.Name,
						OwnerType:     key.OwnerType,
						Enabled:       key.Enabled,
						AllowedAgents: key.AllowedAgents,
						AllowedSkills: key.AllowedSkills,
						RateLimit:     key.RateLimit,
					})
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}

			fmt.Fprintln(out, "ID\tNAME\tENABLED\tALLOWED_AGENTS\tALLOWED_SKILLS\tRATE_LIMIT")
			for _, key := range keys {
				fmt.Fprintf(out, "%s\t%s\t%t\t%s\t%s\t%s\n",
					key.ID, key.Name, key.Enabled,
					strings.Join(key.AllowedAgents, ","),
					strings.Join(key.AllowedSkills, ","),
					key.RateLimit)
			}
			return nil
		},
	}

	flags.register(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func newKeysEnableCommand() *cobra.Command  { return newKeysToggleCommand(true) }
func newKeysDisableCommand() *cobra.Command { return newKeysToggleCommand(false) }

func newKeysToggleCommand(enable bool) *cobra.Command {
	flags := &keysFlags{}
	var id string

	use := "disable"
	short := "Disable a key (emergency revocation)"
	if enable {
		use = "enable"
		short = "Re-enable a disabled key"
	}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := flags.openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			if err := store.SetAPIKeyEnabled(cmd.Context(), id, enable); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: enabled=%t\n", id, enable)
			return nil
		},
	}

	flags.register(cmd)
	cmd.Flags().StringVar(&id, "id", "", "key id")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func newKeysRotateCommand() *cobra.Command {
	flags := &keysFlags{}
	var id string

	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Create a replacement key and disable the old one",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := flags.openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			replacement, plaintext, err := store.RotateAPIKey(cmd.Context(), id)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "old_id:   %s (disabled)\n", id)
			fmt.Fprintf(out, "new_id:   %s\n", replacement.ID)
			fmt.Fprintf(out, "api_key:  %s\n", plaintext)
			fmt.Fprintln(out, "note: switch the caller now; the old key is already disabled")
			return nil
		},
	}

	flags.register(cmd)
	cmd.Flags().StringVar(&id, "id", "", "key id to rotate")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
