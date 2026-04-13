package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/agentstep/mvm/internal/lima"
	"github.com/agentstep/mvm/internal/state"
	"github.com/spf13/cobra"
)

func newTemplateCmd(limaClient *lima.Client, store *state.Store) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "template",
		Short:   "Manage VM templates",
		Aliases: []string{"tpl"},
	}

	cmd.AddCommand(
		newTemplateInitCmd(),
		newTemplateListCmd(),
	)

	return cmd
}

func newTemplateInitCmd() *cobra.Command {
	var preset string

	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Scaffold a new VM template",
		Long: `Create a template directory with a setup script for a specific workload.

  mvm template init my-api --preset node
  mvm template init my-service --preset python
  mvm template init my-db --preset postgres
  mvm template init my-site --preset static`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTemplateInit(args[0], preset)
		},
	}

	cmd.Flags().StringVar(&preset, "preset", "", "template preset: node, python, postgres, static")

	return cmd
}

func newTemplateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available templates",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTemplateList()
		},
	}
}

var presets = map[string]struct {
	description string
	setup       string
}{
	"node": {
		description: "Node.js service with npm",
		setup: `#!/bin/sh
# Node.js template setup — runs inside the VM after boot
set -e
mkdir -p /app
cd /app

# Initialize project if no package.json
if [ ! -f package.json ]; then
    npm init -y
    npm install express
    cat > index.js << 'EOF'
const express = require('express');
const app = express();
const port = process.env.PORT || 3000;

app.get('/', (req, res) => res.json({ status: 'ok' }));
app.listen(port, '0.0.0.0', () => console.log('Listening on ' + port));
EOF
fi

echo "Node.js template ready. Run: cd /app && node index.js"
`,
	},
	"python": {
		description: "Python service with pip",
		setup: `#!/bin/sh
# Python template setup — runs inside the VM after boot
set -e
mkdir -p /app
cd /app

# Initialize project
if [ ! -f main.py ]; then
    pip install flask
    cat > main.py << 'EOF'
from flask import Flask, jsonify
app = Flask(__name__)

@app.route('/')
def health():
    return jsonify(status='ok')

if __name__ == '__main__':
    app.run(host='0.0.0.0', port=5000)
EOF
fi

echo "Python template ready. Run: cd /app && python3 main.py"
`,
	},
	"postgres": {
		description: "PostgreSQL database",
		setup: `#!/bin/sh
# PostgreSQL template setup — runs inside the VM after boot
set -e
apk add --no-cache postgresql postgresql-client
mkdir -p /var/lib/postgresql/data
chown postgres:postgres /var/lib/postgresql/data

su postgres -c "initdb -D /var/lib/postgresql/data" 2>/dev/null || true
su postgres -c "pg_ctl start -D /var/lib/postgresql/data -l /var/log/postgresql.log"

echo "PostgreSQL template ready. Connect: psql -h 172.16.0.2 -U postgres"
`,
	},
	"static": {
		description: "Static file server",
		setup: `#!/bin/sh
# Static site template setup — runs inside the VM after boot
set -e
mkdir -p /srv/www
cat > /srv/www/index.html << 'EOF'
<!DOCTYPE html>
<html><body><h1>mvm static site</h1></body></html>
EOF

echo "Static template ready. Run: cd /srv/www && python3 -m http.server 8080"
`,
	},
}

func runTemplateInit(name, preset string) error {
	if preset == "" {
		fmt.Println("Available presets:")
		for k, v := range presets {
			fmt.Printf("  %-10s %s\n", k, v.description)
		}
		return fmt.Errorf("specify a preset with --preset")
	}

	p, ok := presets[preset]
	if !ok {
		return fmt.Errorf("unknown preset %q. Available: node, python, postgres, static", preset)
	}

	dir := filepath.Join("templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	setupPath := filepath.Join(dir, "setup.sh")
	if err := os.WriteFile(setupPath, []byte(p.setup), 0o755); err != nil {
		return err
	}

	readmePath := filepath.Join(dir, "README.md")
	readme := fmt.Sprintf("# %s\n\nTemplate: %s (%s)\n\nSetup script runs inside the VM after boot:\n```\nmvm start %s\nmvm exec %s -- /bin/sh /app/setup.sh\n```\n", name, preset, p.description, name, name)
	if err := os.WriteFile(readmePath, []byte(readme), 0o644); err != nil {
		return err
	}

	fmt.Printf("Template '%s' created at %s/\n", name, dir)
	fmt.Printf("  Preset: %s (%s)\n", preset, p.description)
	fmt.Printf("  Setup:  %s\n", setupPath)
	fmt.Printf("\nTo use:\n")
	fmt.Printf("  mvm start %s\n", name)
	fmt.Printf("  mvm exec %s -- sh %s/setup.sh\n", name, dir)
	return nil
}

func runTemplateList() error {
	fmt.Println("Built-in presets:")
	for k, v := range presets {
		fmt.Printf("  %-10s %s\n", k, v.description)
	}

	// Check for local templates
	if entries, err := os.ReadDir("templates"); err == nil && len(entries) > 0 {
		fmt.Println("\nLocal templates:")
		for _, e := range entries {
			if e.IsDir() {
				fmt.Printf("  %s/\n", e.Name())
			}
		}
	}
	return nil
}
