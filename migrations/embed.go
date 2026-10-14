// Package migrations embeds the SQLite schema so the binary does not depend on SCHEMA_PATH.
package migrations

import _ "embed"

//go:embed schema.sql
var Schema string
