package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/otx/log"
	"github.com/stretchr/testify/require"
)

// Exercise the configured slog bridge and pretty exporter together: an SDK
// upgrade must not turn startup success into missing or malformed log records.
func TestConfiguredStructuredLogging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	var c OtelConfig
	require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf(`
exporters:
  pretty:
    output_paths: [%q]
providers:
  logger:
    exporters: [pretty]
  meter: {}
`, path)), &c))
	ctx, o, err := c.Build(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Shutdown(context.Background())) })
	require.NoError(t, o.Start(ctx))
	log.From(ctx).Info("structured regression", slog.String("account", "test-user"), slog.Int("count", 3), slog.Group("details", slog.Bool("active", true)))
	require.NoError(t, o.ForceFlush(ctx))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, value := range []string{"structured regression", "account", "test-user", "count", "3", "active", "true"} {
		require.Contains(t, string(b), value)
	}
}
