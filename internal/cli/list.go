package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var (
		jsonOutput bool
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List all microVMs",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(jsonOutput, quiet)
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only print VM names")

	return cmd
}

func runList(jsonOutput, quiet bool) error {
	sc, err := requireDaemon()
	if err != nil {
		return err
	}

	ctx := context.Background()
	vms, err := sc.ListVMs(ctx)
	if err != nil {
		return err
	}

	if len(vms) == 0 {
		if !quiet && !jsonOutput {
			fmt.Println("No microVMs. Create one with: mvm start <name>")
		}
		return nil
	}

	// Sort by creation time
	sort.Slice(vms, func(i, j int) bool {
		return vms[i].CreatedAt.Before(vms[j].CreatedAt)
	})

	if jsonOutput {
		data, _ := json.MarshalIndent(vms, "", "  ")
		fmt.Println(string(data))
		return nil
	}

	if quiet {
		for _, vm := range vms {
			fmt.Println(vm.Name)
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tIP\tCREATED")
	for _, vm := range vms {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			vm.Name, vm.Status, vm.GuestIP, timeAgo(vm.CreatedAt))
	}
	w.Flush()
	return nil
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
