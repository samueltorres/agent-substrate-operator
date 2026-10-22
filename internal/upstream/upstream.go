// Package upstream embeds the Agent Substrate release the operator installs:
// its install manifests and PostgreSQL setup script, copied verbatim by
// hack/sync-upstream.sh. Upstream's Apache-2.0 LICENSE is kept beside them.
package upstream

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed all:files
var files embed.FS

// FS returns the embedded tree, rooted so that "manifests/ate-install/..."
// resolves as it does in the upstream repository.
func FS() fs.FS {
	sub, err := fs.Sub(files, "files")
	if err != nil {
		panic(err)
	}
	return sub
}

// Version is the upstream release tag the embedded files come from.
var Version = strings.TrimSpace(mustRead("VERSION"))

// PostgresSetupSQL is upstream's pkg/postgressetup/setup.sql: the
// parameterized psql script that creates Substrate's roles, users, and schema.
var PostgresSetupSQL = mustRead("setup.sql")

func mustRead(name string) string {
	b, err := fs.ReadFile(FS(), name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
