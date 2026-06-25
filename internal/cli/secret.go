package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentstep/mvm/internal/secrets"
	"github.com/spf13/cobra"
)

func secretStore() *secrets.Store { return secrets.New(mvmDir) }

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted secrets injected into VMs at exec time",
		Long: `Store secrets encrypted at rest (AES-256-GCM) and attach them to a VM at
start. Attached secrets are injected into the env per-exec from host memory —
never written to a guest file. A VM with secrets attached refuses memory
snapshots (the secret could land in the snapshot).

  mvm secret put OPENAI_API_KEY --value sk-...
  echo -n sk-... | mvm secret put OPENAI_API_KEY
  mvm secret list
  mvm secret rm OPENAI_API_KEY
  mvm start dev --secret OPENAI_API_KEY`,
	}
	cmd.AddCommand(newSecretPutCmd(), newSecretListCmd(), newSecretRmCmd())
	return cmd
}

func newSecretPutCmd() *cobra.Command {
	var value string
	cmd := &cobra.Command{
		Use:   "put <name>",
		Short: "Store a secret (value from --value or stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			v := value
			if v == "" {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read value from stdin: %w", err)
				}
				v = strings.TrimRight(string(data), "\n")
			}
			if v == "" {
				return fmt.Errorf("empty value (pass --value or pipe it on stdin)")
			}
			if err := secretStore().Put(name, v); err != nil {
				return err
			}
			fmt.Printf("  ✓ secret %q stored\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "secret value (omit to read from stdin)")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List secret names (never values)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := secretStore().List()
			if err != nil {
				return err
			}
			if len(names) == 0 {
				fmt.Println("no secrets stored")
				return nil
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		},
	}
}

func newSecretRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"delete"},
		Short:   "Delete a secret",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := secretStore().Delete(args[0]); err != nil {
				return err
			}
			fmt.Printf("  ✓ secret %q removed\n", args[0])
			return nil
		},
	}
}

// validateSecretsExist fails fast if any named secret is missing, so a typo
// aborts the start instead of silently injecting nothing.
func validateSecretsExist(names []string) error {
	store := secretStore()
	for _, n := range names {
		ok, err := store.Has(n)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("secret %q not found (store it with: mvm secret put %s)", n, n)
		}
	}
	return nil
}

// secretEnvVars decrypts the named secrets and returns them as KEY=VALUE entries
// suitable for buildExecScript / the startup env prefix. Loaded from host memory
// at call time; the values are never persisted to the guest.
func secretEnvVars(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	store := secretStore()
	out := make([]string, 0, len(names))
	for _, n := range names {
		v, err := store.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, n+"="+v)
	}
	return out, nil
}
