package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ninech/release-mirror/internal/server"
)

// BuildInfo aliases server.BuildInfo for CLI version reporting.
type BuildInfo = server.BuildInfo

// VersionCmd prints the version of the application.
type VersionCmd struct{}

// Run prints version information.
func (v *VersionCmd) Run(_ context.Context, c *cmd) error {
	if _, err := fmt.Fprintln(c.w, version(&c.build)); err != nil {
		return fmt.Errorf("print version: %w", err)
	}
	return nil
}

func version(info *BuildInfo) string {
	details := []string{info.Date, info.Commit, info.GoVersion}
	details = slices.DeleteFunc(details, func(s string) bool { return s == "" })
	return fmt.Sprintf("%s %s (%s)", info.Name, info.Version, strings.Join(details, ", "))
}
