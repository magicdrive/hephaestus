package hephaestus

import (
	"context"
	"flag"
	"io"

	"github.com/magicdrive/hephaestus/internal/publish/cli"
)

// publishCmd runs `hephaestus publish`: release a version of the project in
// the current directory by pushing its tag, then follow and verify the
// release workflow (internal/publish, docs/publish.md). The gh token it
// reads for the generic preflight is redacted from the output.
func publishCmd(ctx context.Context, args []string, stdout, stderr io.Writer, red *redactor) int {
	return cli.Run(ctx, args, stdout, stderr, cli.Config{
		Name:           "hephaestus publish",
		Usage:          cli.CommandUsage,
		Help:           cli.CommandHelp,
		ImpliedPublish: true,
		DirectHints:    true,
		Redact:         func(s string) { red.add(s) },
	})
}

// publishFlags are the flags of `hephaestus publish`, for completion.
func publishFlags() *flag.FlagSet {
	fs, _ := cli.NewFlagSet("publish")
	return fs
}

// publishSubcommands are the commands of `hephaestus publish`, for
// completion.
func publishSubcommands() []candidate {
	var out []candidate
	for _, c := range cli.Commands {
		out = append(out, candidate{c.Name, c.Summary})
	}
	return out
}
